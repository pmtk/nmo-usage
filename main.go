package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	nmov1beta1 "github.com/medik8s/node-maintenance-operator/api/v1beta1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	discovery "k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	nmGV   = nmov1beta1.GroupVersion.String()
	scheme = k8sruntime.NewScheme()
)

func init() {
	utilruntime.Must(nmov1beta1.AddToScheme(scheme))
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	cfg := getConfig()
	checkIfNMOInstalled(cfg)

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	worker := getWorker(c, "")
	log.Printf("Picked worker: %s\n", worker.Name)
	printWorkerStatus(c, worker.Name)

	wg := &sync.WaitGroup{}
	wg.Add(2)
	go simulateOperator(c, worker.Name, wg, "Operator 1 > ")
	time.Sleep(1 * time.Second)
	go simulateOperator(c, worker.Name, wg, "Operator 2 > ")
	wg.Wait()
}

func printWorkerStatus(c client.Client, name string) {
	w := getWorker(c, name)
	log.Printf("Node: %s   Schedulable: %v\n", w.Name, !w.Spec.Unschedulable)
}

func simulateOperator(c client.Client, workerName string, wg *sync.WaitGroup, prefix string) {
	logger := log.New(os.Stdout, prefix, log.Flags())
	logger.Println("Starting Operator")

	nm := &nmov1beta1.NodeMaintenance{
		ObjectMeta: metav1.ObjectMeta{Name: "nm-test"},
		Spec: nmov1beta1.NodeMaintenanceSpec{
			NodeName: workerName,
			Reason:   "Testing NMO",
		},
	}

	backoff := wait.Backoff{Steps: 10, Duration: 10 * time.Second, Factor: 1}
	if err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		err := c.Create(context.Background(), nm)
		if err != nil {
			if strings.Contains(err.Error(), fmt.Sprintf("a NodeMaintenance for node %s already exists", workerName)) {
				logger.Printf("NodeMaintenance CR already exists for the node")
				return false, nil
			}
			logger.Printf("Failed to create NodeMaintenance CR: %v", err)
			return false, err
		}
		return true, nil
	}); err != nil {
		if err == wait.ErrWaitTimeout {
			logger.Printf("Waiting to create a NM CR timed out\n")
		}
		logger.Printf("Failed to create NodeMaintenance CR: %v", err)
		return
	}
	logger.Printf("Created NodeMaintenance CR - waiting for drained node")

	printWorkerStatus(c, workerName)

	for {
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(nm), nm); err != nil {
			logger.Printf("Failed to get NodeMaintenance CR: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		printWorkerStatus(c, workerName)
		logger.Printf("Phase:%v    Total Pods:%v    Eviction Pods:%v    Pending Pods:%v    Last Error:%v \n",
			nm.Status.Phase, nm.Status.TotalPods, nm.Status.EvictionPods, len(nm.Status.PendingPods), nm.Status.LastError)

		if nm.Status.Phase == nmov1beta1.MaintenanceSucceeded {
			logger.Printf("Drain successful\n")
			break
		}
		time.Sleep(5 * time.Second)
	}

	logger.Printf("Working for 15 seconds\n")
	time.Sleep(15 * time.Second)

	printWorkerStatus(c, workerName)
	logger.Println("Finished work - deleting NodeMaintenance CR")
	if err := c.Delete(context.Background(), nm); err != nil {
		logger.Fatalf("Failed to delete NodeMaintenance CR: %v", err)
	}
	printWorkerStatus(c, workerName)
	wg.Done()
}

func getConfig() *rest.Config {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		log.Fatalln("Env var KUBECONFIG is empty")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		log.Fatalf("Failed to build config from kubeconfig: %v", err)
	}
	return cfg
}

func checkIfNMOInstalled(cfg *rest.Config) {
	discoveryClient := discovery.NewDiscoveryClientForConfigOrDie(cfg)
	nmSR, err := discoveryClient.ServerResourcesForGroupVersion(nmGV)
	if err != nil {
		log.Fatalf("Failed to discover ServerResources: %v\nIs Node Maintenance Operator installed?\n", err)
	}
	if nmSR == nil {
		log.Fatalf("Could not get %s ServerResource\nIs Node Maintenance Operator installed?\n", nmGV)
	}
	log.Printf("Discovered %s\n", nmGV)
}

func getWorker(c client.Client, name string) corev1.Node {
	listOpt := &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{"node-role.kubernetes.io/worker": ""}),
	}

	if name != "" {
		listOpt.FieldSelector = fields.OneTermEqualSelector("metadata.name", name)
	}

	nodes := &corev1.NodeList{}
	if err := c.List(context.Background(), nodes, listOpt); err != nil {
		log.Fatalf("Failed to list nodes: %+v\n", err)
	}
	if len(nodes.Items) == 0 {
		log.Fatalln("Discovered 0 workers.")
	}
	if name != "" && len(nodes.Items) != 1 {
		log.Fatalf("Discovered %v workers, but expected 1\n", len(nodes.Items))
	}

	return nodes.Items[0]
}
