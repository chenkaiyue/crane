package resource

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/robfig/cron/v3"

	predictionv1 "github.com/gocrane/api/pkg/generated/informers/externalversions/prediction/v1alpha1"
	predictionlisters "github.com/gocrane/api/pkg/generated/listers/prediction/v1alpha1"
	predictionapi "github.com/gocrane/api/prediction/v1alpha1"
	"github.com/gocrane/crane/pkg/common"
	"github.com/gocrane/crane/pkg/ensurance/collector/types"
	"github.com/gocrane/crane/pkg/known"
	"github.com/gocrane/crane/pkg/metrics"
	"github.com/gocrane/crane/pkg/utils"
)

const (
	MinDeltaRatio                                 = 0.1
	StateExpiration                               = 1 * time.Minute
	TspUpdateInterval                             = 20 * time.Second
	NodeReserveResourcePercentageAnnotationPrefix = "reserve.node.gocrane.io/%s"

	NodeExtResourceAnnotationPrefix = "ext.node.gocrane.io/%s"
	// ExtEnable whether to reuse the extended idle resource
	ExtEnable = "enabled"
	// TimeDivisionStart is the time to start reuse the extended idle resource
	TimeDivisionStart = "timeDivisionStart"
	// TimeDivisionEnd is the time to stop reusing the extended idle resource
	TimeDivisionEnd = "timeDivisionEnd"
	// EvictPodEnabled is whether to evict pods which use extended idle resource when TimeDivisionEnd
	EvictPod = "evictPodEnabled"
)

var idToResourceMap = map[string]v1.ResourceName{
	v1.ResourceCPU.String():    v1.ResourceCPU,
	v1.ResourceMemory.String(): v1.ResourceMemory,
}

// ReserveResource is the cpu and memory reserve configuration
type ReserveResource struct {
	CpuPercent *float64
	MemPercent *float64
}

type NodeResourceManager struct {
	nodeName string
	client   clientset.Interface

	nodeLister corelisters.NodeLister
	nodeSynced cache.InformerSynced

	podLister corelisters.PodLister
	podSynced cache.InformerSynced

	tspLister predictionlisters.TimeSeriesPredictionLister
	tspSynced cache.InformerSynced

	recorder record.EventRecorder

	stateChann chan map[string][]common.TimeSeries

	state map[string][]common.TimeSeries
	// Updated when get new data from stateChann, used to determine whether state has expired
	lastStateTime time.Time

	reserveResource ReserveResource

	tspName string

	timeDivisionCur bool

	timeDivisionEnabled  bool
	timeDivisionStart    string
	timeDivisionEnd      string
	timeDivisionEvictPod bool

	startCronJob *cron.Cron
	endCronJob   *cron.Cron
}

func NewNodeResourceManager(client clientset.Interface, nodeName string, nodeResourceReserved map[string]string, tspName string, nodeInformer coreinformers.NodeInformer,
	podInformer coreinformers.PodInformer, tspInformer predictionv1.TimeSeriesPredictionInformer, stateChann chan map[string][]common.TimeSeries) (*NodeResourceManager, error) {
	reserveCpuPercent, err := utils.ParsePercentage(nodeResourceReserved[v1.ResourceCPU.String()])
	if err != nil {
		return nil, err
	}
	reserveMemoryPercent, err := utils.ParsePercentage(nodeResourceReserved[v1.ResourceMemory.String()])
	if err != nil {
		return nil, err
	}

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: client.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "crane-agent"})

	o := &NodeResourceManager{
		nodeName:   nodeName,
		client:     client,
		nodeLister: nodeInformer.Lister(),
		nodeSynced: nodeInformer.Informer().HasSynced,
		podLister:  podInformer.Lister(),
		podSynced:  podInformer.Informer().HasSynced,
		tspLister:  tspInformer.Lister(),
		tspSynced:  tspInformer.Informer().HasSynced,
		recorder:   recorder,
		stateChann: stateChann,
		reserveResource: ReserveResource{
			CpuPercent: &reserveCpuPercent,
			MemPercent: &reserveMemoryPercent,
		},
		tspName:         tspName,
		timeDivisionCur: true,
	}
	nodeInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		// Focused on pod belonged to this node
		FilterFunc: func(obj interface{}) bool {
			switch t := obj.(type) {
			case *v1.Node:
				return t.Name == o.nodeName
			default:
				utilruntime.HandleError(fmt.Errorf("unable to handle object %T", obj))
				return false
			}
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: o.reconcileCron,
			UpdateFunc: func(old, cur interface{}) {
				o.reconcileCron(cur)
			},
		},
	})
	return o, nil
}

func (o *NodeResourceManager) reconcileCron(obj interface{}) {
	timeDivisionEnabledAnno, timeDivisionEvictPod, timeDivisionStart, timeDivisionEnd := o.getNodeTimeDivisionAnnotations()

	if timeDivisionEnabledAnno != o.timeDivisionEnabled || timeDivisionEvictPod != o.timeDivisionEvictPod || timeDivisionStart != o.timeDivisionStart || timeDivisionEnd != o.timeDivisionEnd {
		if o.startCronJob != nil {
			o.startCronJob.Stop()
		}
		if o.endCronJob != nil {
			o.endCronJob.Stop()
		}

		if timeDivisionEnabledAnno {
			o.StartCronCollect()
		}
	}
}

func (o *NodeResourceManager) Name() string {
	return "NodeResourceManager"
}

func (o *NodeResourceManager) Run(stop <-chan struct{}) {
	klog.Infof("Starting node resource manager.")

	// Wait for the caches to be synced before starting workers
	if !cache.WaitForNamedCacheSync("node-resource-manager",
		stop,
		o.tspSynced,
		o.nodeSynced,
		o.podSynced,
	) {
		return
	}

	go func() {
		tspUpdateTicker := time.NewTicker(TspUpdateInterval)
		defer tspUpdateTicker.Stop()
		for {
			select {
			case state := <-o.stateChann:
				if !(o.getExtEnabledFromNodeAnnotations() && o.timeDivisionCur) {
					continue
				}
				o.state = state
				o.lastStateTime = time.Now()
				start := time.Now()
				metrics.UpdateLastTime(string(known.ModuleNodeResourceManager), metrics.StepUpdateNodeResource, start)
				o.UpdateNodeResource()
				metrics.UpdateDurationFromStart(string(known.ModuleNodeResourceManager), metrics.StepUpdateNodeResource, start)
			case <-stop:
				klog.Infof("node resource manager exit")
				return
			}
		}
	}()

	return
}

func (o *NodeResourceManager) UpdateNodeResource() {
	node := o.getNode()
	if len(node.Status.Addresses) == 0 {
		klog.Error("Node addresses is empty")
		return
	}
	nodeCopy := node.DeepCopy()

	resourcesFrom := o.BuildNodeStatus(nodeCopy)
	if !equality.Semantic.DeepEqual(&node.Status, &nodeCopy.Status) {
		// Update Node status extend-resource info
		// TODO fix: strategic merge patch kubernetes
		if _, err := o.client.CoreV1().Nodes().UpdateStatus(context.TODO(), nodeCopy, metav1.UpdateOptions{}); err != nil {
			klog.Errorf("Failed to update node %s extended resource, %v", nodeCopy.Name, err)
			return
		}
		klog.V(2).Infof("Update node %s extended resource successfully", node.Name)
		o.recorder.Event(node, v1.EventTypeNormal, "UpdateNode", generateUpdateEventMessage(resourcesFrom))
	}
}

func (o *NodeResourceManager) getNode() *v1.Node {
	node, err := o.nodeLister.Get(o.nodeName)
	if err != nil {
		klog.Errorf("Failed to get node: %v", err)
		return nil
	}
	return node
}

func (o *NodeResourceManager) StartCronCollect() {
	start_time, end_time := o.getExtTimeFromNodeAnnotations()
	o.timeDivisionStart = start_time
	o.timeDivisionEnd = end_time
	//start_time := "00 " + "00 " + strconv.Itoa(o.timeDivisionStart) + " * * ?"
	if start_time != "" {
		start := cron.New()
		start.AddFunc(start_time, func() {
			o.timeDivisionCur = true
		})
		o.startCronJob = start

		start.Start()
	}

	if end_time != "" {
		end := cron.New()
		//end_time := "00 " + "00 " + strconv.Itoa(o.timeDivisionEnd) + " * * ?"
		end.AddFunc(end_time, func() {
			o.timeDivisionCur = false

			evict := o.getExtEvcitEnabledFromNodeAnnotations()
			o.timeDivisionEvictPod = evict

			if evict {
				o.deleteAllExtPod()
			}
			o.clearNodeExtResources()
		})
		o.endCronJob = end
		end.Start()
	}
}

func (o *NodeResourceManager) FindTargetNode(tsp *predictionapi.TimeSeriesPrediction, addresses []v1.NodeAddress) (bool, error) {
	address := tsp.Spec.TargetRef.Name
	if address == "" {
		return false, fmt.Errorf("tsp %s target is not specified", tsp.Name)
	}

	// the reason we use node ip instead of node name as the target name is
	// some monitoring system does not persist node name
	for _, addr := range addresses {
		if addr.Address == address {
			return true, nil
		}
	}
	klog.V(4).Infof("Target %s mismatch this node", address)
	return false, nil
}

func (o *NodeResourceManager) BuildNodeStatus(node *v1.Node) map[string]int64 {
	tspCanNotBeReclaimedResource := o.GetCanNotBeReclaimedResourceFromTsp(node)
	localCanNotBeReclaimedResource := o.GetCanNotBeReclaimedResourceFromLocal()

	reserveCpuPercent := o.reserveResource.CpuPercent
	if nodeReserveCpuPercent, ok := getReserveResourcePercentFromNodeAnnotations(node.GetAnnotations(), v1.ResourceCPU.String()); ok {
		reserveCpuPercent = &nodeReserveCpuPercent
	}

	reserveMemPercent := o.reserveResource.MemPercent
	if nodeReserveMemPercent, ok := getReserveResourcePercentFromNodeAnnotations(node.GetAnnotations(), v1.ResourceMemory.String()); ok {
		reserveMemPercent = &nodeReserveMemPercent
	}

	extResourceFrom := map[string]int64{}

	for resourceName, value := range tspCanNotBeReclaimedResource {
		klog.V(6).Infof("resourcename is %s", resourceName)
		resourceFrom := "tsp"
		maxUsage := value
		if localCanNotBeReclaimedResource[resourceName] > maxUsage {
			maxUsage = localCanNotBeReclaimedResource[resourceName]
			resourceFrom = "local"
		}

		var nextRecommendation float64
		switch resourceName {
		case v1.ResourceCPU:
			if *reserveCpuPercent != 0 {
				nextRecommendation = float64(node.Status.Allocatable.Cpu().Value()) - float64(node.Status.Allocatable.Cpu().Value())*(*reserveCpuPercent) - maxUsage/1000
			} else {
				nextRecommendation = float64(node.Status.Allocatable.Cpu().Value()) - maxUsage/1000
			}
		case v1.ResourceMemory:
			// unit of memory in prometheus is in Ki, need to be converted to byte
			if *reserveMemPercent != 0 {
				nextRecommendation = float64(node.Status.Allocatable.Memory().Value()) - float64(node.Status.Allocatable.Memory().Value())*(*reserveMemPercent) - maxUsage/1000
			} else {
				klog.V(6).Infof("allocatable mem is %d, maxusage is %f", node.Status.Allocatable.Memory().Value(), maxUsage)
				nextRecommendation = float64(node.Status.Allocatable.Memory().Value()) - maxUsage
			}
		default:
			continue
		}
		if nextRecommendation < 0 {
			nextRecommendation = 0
		}
		metrics.UpdateNodeResourceRecommendedValue(metrics.SubComponentNodeResource, metrics.StepGetExtResourceRecommended, string(resourceName), resourceFrom, nextRecommendation)
		extResourceName := fmt.Sprintf(utils.ExtResourcePrefixFormat, string(resourceName))
		resValue, exists := node.Status.Capacity[v1.ResourceName(extResourceName)]
		if exists && resValue.Value() != 0 &&
			math.Abs(float64(resValue.Value())-
				nextRecommendation)/float64(resValue.Value()) <= MinDeltaRatio {
			continue
		}
		switch resourceName {
		case v1.ResourceCPU:
			node.Status.Capacity[v1.ResourceName(extResourceName)] =
				*resource.NewQuantity(int64(nextRecommendation), resource.DecimalSI)
			node.Status.Allocatable[v1.ResourceName(extResourceName)] =
				*resource.NewQuantity(int64(nextRecommendation), resource.DecimalSI)
		case v1.ResourceMemory:
			node.Status.Capacity[v1.ResourceName(extResourceName)] =
				*resource.NewQuantity(int64(nextRecommendation), resource.BinarySI)
			node.Status.Allocatable[v1.ResourceName(extResourceName)] =
				*resource.NewQuantity(int64(nextRecommendation), resource.BinarySI)
		}

		extResourceFrom[resourceFrom+"-"+resourceName.String()] = int64(nextRecommendation)
	}

	return extResourceFrom
}

func (o *NodeResourceManager) GetCanNotBeReclaimedResourceFromTsp(node *v1.Node) map[v1.ResourceName]float64 {
	canNotBeReclaimedResource := map[v1.ResourceName]float64{
		v1.ResourceCPU:    0,
		v1.ResourceMemory: 0,
	}

	tsp, err := o.tspLister.TimeSeriesPredictions(known.CraneSystemNamespace).Get(o.tspName)
	if err != nil {
		klog.Errorf("Failed to get tsp: %#v", err)
		return canNotBeReclaimedResource
	}

	tspMatched, err := o.FindTargetNode(tsp, node.Status.Addresses)
	if err != nil {
		klog.Error(err.Error())
		return canNotBeReclaimedResource
	}

	if !tspMatched {
		klog.Errorf("Found tsp %s, but tsp not matched to node %s", o.tspName, node.Name)
		return canNotBeReclaimedResource
	}

	// build node status
	nextPredictionResourceStatus := &tsp.Status
	for _, predictionMetric := range nextPredictionResourceStatus.PredictionMetrics {
		resourceName, exists := idToResourceMap[predictionMetric.ResourceIdentifier]
		if !exists {
			continue
		}
		for _, timeSeries := range predictionMetric.Prediction {
			var nextUsage float64
			var nextUsageFloat float64
			var err error
			for _, sample := range timeSeries.Samples {
				if nextUsageFloat, err = strconv.ParseFloat(sample.Value, 64); err != nil {
					klog.Errorf("Failed to parse extend resource value %v: %v", sample.Value, err)
					continue
				}
				nextUsage = nextUsageFloat
				if canNotBeReclaimedResource[resourceName] < nextUsage {
					canNotBeReclaimedResource[resourceName] = nextUsage
				}
			}
		}
	}
	return canNotBeReclaimedResource
}

func (o *NodeResourceManager) GetCanNotBeReclaimedResourceFromLocal() map[v1.ResourceName]float64 {
	return map[v1.ResourceName]float64{
		v1.ResourceCPU:    o.GetCpuCoreCanNotBeReclaimedFromLocal(),
		v1.ResourceMemory: o.GetMemCanNotBeReclaimedFromLocal(),
	}
}

func (o *NodeResourceManager) GetMemCanNotBeReclaimedFromLocal() float64 {
	var memUsageTotal float64
	memUsage, ok := o.state[string(types.MetricNameMemoryTotalUsage)]
	if ok {
		memUsageTotal = memUsage[0].Samples[0].Value
		klog.V(4).Infof("%s: %f", types.MetricNameMemoryTotalUsage, memUsageTotal)

	} else {
		klog.V(4).Infof("Can't get %s from NodeResourceManager local state", types.MetricNameMemoryTotalUsage)
	}

	var extResContainerMemUsageTotal float64 = 0
	extResContainerCpuUsageTotalTimeSeries, ok := o.state[string(types.MetricNameExtResContainerMemTotalUsage)]
	if ok {
		extResContainerMemUsageTotal = extResContainerCpuUsageTotalTimeSeries[0].Samples[0].Value
	} else {
		klog.V(4).Infof("Can't get %s from NodeResourceManager local state", types.MetricNameExtResContainerCpuTotalUsage)
	}

	klog.V(6).Infof("nodeMemUsageTotal: %f, extResContainerMemUsageTotal: %f", memUsageTotal, extResContainerMemUsageTotal)

	// 1. Exclusive tethered CPU cannot be reclaimed even if the free part is free, so add the exclusive CPUIdle to the CanNotBeReclaimed CPU
	// 2. The CPU used by extRes-container needs to be reclaimed, otherwise it will be double-counted due to the allotted mechanism of k8s, so the extResContainerCpuUsageTotal is subtracted from the CanNotBeReclaimedCpu
	nodeMemCannotBeReclaimedSeconds := memUsageTotal - extResContainerMemUsageTotal

	metrics.UpdateNodeMemCannotBeReclaimedSeconds(nodeMemCannotBeReclaimedSeconds)
	return nodeMemCannotBeReclaimedSeconds
}

func (o *NodeResourceManager) GetCpuCoreCanNotBeReclaimedFromLocal() float64 {
	if o.lastStateTime.Before(time.Now().Add(-20 * time.Second)) {
		klog.V(1).Infof("NodeResourceManager local state has expired")
		return 0
	}

	nodeCpuUsageTotalTimeSeries, ok := o.state[string(types.MetricNameCpuTotalUsage)]
	if !ok {
		klog.V(4).Infof("Can't get %s from NodeResourceManager local state, please make sure cpu metrics collector is defined in NodeQOS.", types.MetricNameCpuTotalUsage)
		return 0
	}
	nodeCpuUsageTotal := nodeCpuUsageTotalTimeSeries[0].Samples[0].Value

	var extResContainerCpuUsageTotal float64 = 0
	extResContainerCpuUsageTotalTimeSeries, ok := o.state[string(types.MetricNameExtResContainerCpuTotalUsage)]
	if ok {
		extResContainerCpuUsageTotal = extResContainerCpuUsageTotalTimeSeries[0].Samples[0].Value * 1000
	} else {
		klog.V(4).Infof("Can't get %s from NodeResourceManager local state", types.MetricNameExtResContainerCpuTotalUsage)
	}

	var exclusiveCPUIdle float64 = 0
	exclusiveCPUIdleTimeSeries, ok := o.state[string(types.MetricNameExclusiveCPUIdle)]
	if ok {
		exclusiveCPUIdle = exclusiveCPUIdleTimeSeries[0].Samples[0].Value
	} else {
		klog.V(4).Infof("Can't get %s from NodeResourceManager local state", types.MetricNameExclusiveCPUIdle)
	}

	klog.V(6).Infof("nodeCpuUsageTotal: %f, exclusiveCPUIdle: %f, extResContainerCpuUsageTotal: %f", nodeCpuUsageTotal, exclusiveCPUIdle, extResContainerCpuUsageTotal)

	// 1. Exclusive tethered CPU cannot be reclaimed even if the free part is free, so add the exclusive CPUIdle to the CanNotBeReclaimed CPU
	// 2. The CPU used by extRes-container needs to be reclaimed, otherwise it will be double-counted due to the allotted mechanism of k8s, so the extResContainerCpuUsageTotal is subtracted from the CanNotBeReclaimedCpu
	nodeCpuCannotBeReclaimedSeconds := nodeCpuUsageTotal + exclusiveCPUIdle - extResContainerCpuUsageTotal
	metrics.UpdateNodeCpuCannotBeReclaimedSeconds(nodeCpuCannotBeReclaimedSeconds)
	return nodeCpuCannotBeReclaimedSeconds
}

func (o *NodeResourceManager) deleteAllExtPod() {
	labelSelector := labels.NewSelector()
	pods, err := o.podLister.List(labelSelector)
	if err != nil {
		klog.Errorf("Failed to list pods for deleting ext pods")
		return
	}
	for _, pod := range pods {
		if HasExtCpuRes(pod) {
			if err = o.client.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{}); err != nil {
				klog.Errorf("Failed to delete ext pods %s/%s", pod.Namespace, pod.Name)
			}
		}
	}
}
func (o *NodeResourceManager) clearNodeExtResources() {
	node := o.getNode()
	nodeCopy := node.DeepCopy()
	extCPUResourceName := fmt.Sprintf(utils.ExtResourcePrefixFormat, v1.ResourceCPU)
	extMemResourceName := fmt.Sprintf(utils.ExtResourcePrefixFormat, v1.ResourceMemory)
	nodeCopy.Status.Capacity[v1.ResourceName(extCPUResourceName)] = *resource.NewQuantity(0, resource.DecimalSI)
	nodeCopy.Status.Capacity[v1.ResourceName(extMemResourceName)] = *resource.NewQuantity(0, resource.BinarySI)

	if _, err := o.client.CoreV1().Nodes().UpdateStatus(context.TODO(), nodeCopy, metav1.UpdateOptions{}); err != nil {
		klog.Errorf("Failed to clear node %s extended resource, %v", nodeCopy.Name, err)
		return
	}
}

func getReserveResourcePercentFromNodeAnnotations(annotations map[string]string, resourceName string) (float64, bool) {
	if annotations == nil {
		return 0, false
	}
	var reserveResourcePercentStr string
	var ok = false
	switch resourceName {
	case v1.ResourceCPU.String():
		reserveResourcePercentStr, ok = annotations[fmt.Sprintf(NodeReserveResourcePercentageAnnotationPrefix, v1.ResourceCPU.String())]
	case v1.ResourceMemory.String():
		reserveResourcePercentStr, ok = annotations[fmt.Sprintf(NodeReserveResourcePercentageAnnotationPrefix, v1.ResourceMemory.String())]
	default:
	}
	if !ok {
		return 0, false
	}

	reserveResourcePercent, err := utils.ParsePercentage(reserveResourcePercentStr)
	if err != nil {
		return 0, false
	}

	return reserveResourcePercent, ok
}

func (o *NodeResourceManager) getExtEnabledFromNodeAnnotations() bool {
	node := o.getNode()
	if len(node.Status.Addresses) == 0 {
		klog.Error("Node addresses is empty")
		return true
	}
	nodeCopy := node.DeepCopy()
	annotations := nodeCopy.GetAnnotations()
	if annotations == nil {
		return true
	}
	enabled, ok := annotations[fmt.Sprintf(NodeExtResourceAnnotationPrefix, ExtEnable)]
	if !ok || enabled == "true" {
		return true
	}
	return false
}

func (o *NodeResourceManager) getExtTimeFromNodeAnnotations() (start, end string) {
	node := o.getNode()
	if len(node.Status.Addresses) == 0 {
		klog.Error("Node addresses is empty")
		return "", ""
	}

	nodeCopy := node.DeepCopy()
	annotations := nodeCopy.GetAnnotations()

	if annotations == nil {
		return "", ""
	}

	s, ok := annotations[fmt.Sprintf(NodeExtResourceAnnotationPrefix, TimeDivisionStart)]
	if !ok {
		start = ""
	} else {
		start = s
	}

	e, ok := annotations[fmt.Sprintf(NodeExtResourceAnnotationPrefix, TimeDivisionEnd)]
	if !ok {
		end = ""
	} else {
		end = e
	}

	return start, end
}

func (o *NodeResourceManager) getExtEvcitEnabledFromNodeAnnotations() bool {
	node := o.getNode()
	if len(node.Status.Addresses) == 0 {
		klog.Error("Node addresses is empty")
		return true
	}

	nodeCopy := node.DeepCopy()
	annotations := nodeCopy.GetAnnotations()

	if annotations == nil {
		return true
	}

	enabled, ok := annotations[fmt.Sprintf(NodeExtResourceAnnotationPrefix, EvictPod)]
	if !ok || enabled == "true" {
		return true
	}

	return false
}

func (o *NodeResourceManager) getNodeTimeDivisionAnnotations() (timeDivisionEnabledAnno, timeDivisionEvictPod bool, timeDivisionStart, timeDivisionEnd string) {
	node := o.getNode()

	nodeCopy := node.DeepCopy()
	annotations := nodeCopy.GetAnnotations()

	if annotations == nil {
		return true, true, "", ""
	}

	timeDivisionEnabledAnno = o.getExtEnabledFromNodeAnnotations()
	timeDivisionStart, timeDivisionEnd = o.getExtTimeFromNodeAnnotations()
	timeDivisionEvictPod = o.getExtEvcitEnabledFromNodeAnnotations()

	return
}

func generateUpdateEventMessage(resourcesFrom map[string]int64) string {
	message := ""
	for k, v := range resourcesFrom {
		message = message + fmt.Sprintf("Updating elastic resource %s with %d.", k, v)
	}
	return message
}

// HasExtCpuRes whether pod has gocrane.io resources
func HasExtCpuRes(pod *v1.Pod) bool {
	for _, v := range pod.Spec.Containers {
		for res, val := range v.Resources.Limits {
			if (strings.HasPrefix(res.String(), fmt.Sprintf(utils.ExtResourcePrefixFormat, v1.ResourceCPU)) && val.Value() != 0) ||
				(strings.HasPrefix(res.String(), fmt.Sprintf(utils.ExtResourcePrefixFormat, v1.ResourceMemory)) && val.Value() != 0) {
				return true
			}
		}
		for res, val := range v.Resources.Requests {
			if strings.HasPrefix(res.String(), fmt.Sprintf(utils.ExtResourcePrefixFormat, v1.ResourceCPU)) && val.Value() != 0 ||
				(strings.HasPrefix(res.String(), fmt.Sprintf(utils.ExtResourcePrefixFormat, v1.ResourceMemory)) && val.Value() != 0) {
				return true
			}
		}
	}
	return false
}
