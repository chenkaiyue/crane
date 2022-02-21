package agent

import (
	"context"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"

	ensuranceapi "github.com/gocrane/api/ensurance/v1alpha1"
	craneclientset "github.com/gocrane/api/pkg/generated/clientset/versioned"
	"github.com/gocrane/api/pkg/generated/informers/externalversions/ensurance/v1alpha1"

	"github.com/gocrane/crane/pkg/ensurance/analyzer"
	"github.com/gocrane/crane/pkg/ensurance/collector"
	"github.com/gocrane/crane/pkg/ensurance/executor"
	"github.com/gocrane/crane/pkg/ensurance/manager"
)

type Agent struct {
	ctx         context.Context
	name        string
	kubeClient  kubernetes.Interface
	craneClient craneclientset.Interface
	managers    []manager.Manager
}

func NewAgent(ctx context.Context,
	nodeName, runtimeEndpoint string,
	kubeClient *kubernetes.Clientset,
	craneClient *craneclientset.Clientset,
	podInformer coreinformers.PodInformer,
	nodeInformer coreinformers.NodeInformer,
	nepInformer v1alpha1.NodeQOSEnsurancePolicyInformer,
	actionInformer v1alpha1.AvoidanceActionInformer,
	ifaces []string,
) (*Agent, error) {
	var managers []manager.Manager
	var noticeCh = make(chan executor.AvoidanceExecutor)

	utilruntime.Must(ensuranceapi.AddToScheme(scheme.Scheme))

	stateCollector := collector.NewStateCollector(nodeName, nepInformer.Lister(), podInformer.Lister(), nodeInformer.Lister(), ifaces)
	managers = append(managers, stateCollector)
	analyzerManager := analyzer.NewAnormalyAnalyzer(kubeClient, nodeName, podInformer, nodeInformer, nepInformer, actionInformer, stateCollector.StateChann, noticeCh)
	managers = append(managers, analyzerManager)
	avoidanceManager := executor.NewActionExecutor(kubeClient, nodeName, podInformer, nodeInformer, noticeCh, runtimeEndpoint)
	managers = append(managers, avoidanceManager)

	return &Agent{
		ctx:         ctx,
		name:        getAgentName(nodeName),
		kubeClient:  kubeClient,
		craneClient: craneClient,
		managers:    managers,
	}, nil
}

func (a *Agent) Run() {
	klog.Infof("Crane agent %s is starting", a.name)

	for _, m := range a.managers {
		m.Run(a.ctx.Done())
	}

	<-a.ctx.Done()

}

func getAgentName(nodeName string) string {
	return nodeName + "_" + string(uuid.NewUUID())
}
