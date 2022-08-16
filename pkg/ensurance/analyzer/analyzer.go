package analyzer

import (
	"container/heap"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	ensuranceapi "github.com/gocrane/api/ensurance/v1alpha1"
	"github.com/gocrane/api/pkg/generated/informers/externalversions/ensurance/v1alpha1"
	ensurancelisters "github.com/gocrane/api/pkg/generated/listers/ensurance/v1alpha1"
	"github.com/gocrane/crane/pkg/common"
	"github.com/gocrane/crane/pkg/ensurance/analyzer/evaluator"
	ecache "github.com/gocrane/crane/pkg/ensurance/cache"
	"github.com/gocrane/crane/pkg/ensurance/executor"
	podinfo "github.com/gocrane/crane/pkg/ensurance/executor/podinfo"
	"github.com/gocrane/crane/pkg/known"
	"github.com/gocrane/crane/pkg/metrics"
	"github.com/gocrane/crane/pkg/utils"
)

type AnormalyAnalyzer struct {
	nodeName string

	podLister corelisters.PodLister
	podSynced cache.InformerSynced

	nodeLister corelisters.NodeLister
	nodeSynced cache.InformerSynced

	nodeQOSLister ensurancelisters.NodeQOSLister
	nodeQOSSynced cache.InformerSynced

	podQOSLister ensurancelisters.PodQOSLister
	podQOSSynced cache.InformerSynced

	avoidanceActionLister ensurancelisters.AvoidanceActionLister
	avoidanceActionSynced cache.InformerSynced

	stateChann chan map[string][]common.TimeSeries
	recorder   record.EventRecorder
	actionCh   chan<- executor.AvoidanceExecutor

	evaluator         evaluator.Evaluator
	triggered         map[string]uint64
	restored          map[string]uint64
	actionEventStatus map[string]ecache.DetectionStatus
	lastTriggeredTime time.Time
}

// NewAnormalyAnalyzer create an analyzer manager
func NewAnormalyAnalyzer(kubeClient *kubernetes.Clientset,
	nodeName string,
	podInformer coreinformers.PodInformer,
	nodeInformer coreinformers.NodeInformer,
	nodeQOSInformer v1alpha1.NodeQOSInformer,
	podQOSInformer v1alpha1.PodQOSInformer,
	actionInformer v1alpha1.AvoidanceActionInformer,
	stateChann chan map[string][]common.TimeSeries,
	noticeCh chan<- executor.AvoidanceExecutor,
) *AnormalyAnalyzer {

	expressionEvaluator := evaluator.NewExpressionEvaluator()
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "crane-agent"})

	return &AnormalyAnalyzer{
		nodeName:              nodeName,
		evaluator:             expressionEvaluator,
		actionCh:              noticeCh,
		recorder:              recorder,
		podLister:             podInformer.Lister(),
		podSynced:             podInformer.Informer().HasSynced,
		nodeLister:            nodeInformer.Lister(),
		nodeSynced:            nodeInformer.Informer().HasSynced,
		nodeQOSLister:         nodeQOSInformer.Lister(),
		nodeQOSSynced:         nodeQOSInformer.Informer().HasSynced,
		podQOSLister:          podQOSInformer.Lister(),
		podQOSSynced:          podQOSInformer.Informer().HasSynced,
		avoidanceActionLister: actionInformer.Lister(),
		avoidanceActionSynced: actionInformer.Informer().HasSynced,
		stateChann:            stateChann,
		triggered:             make(map[string]uint64),
		restored:              make(map[string]uint64),
		actionEventStatus:     make(map[string]ecache.DetectionStatus),
	}
}

func (s *AnormalyAnalyzer) Name() string {
	return "AnormalyAnalyzer"
}

func (s *AnormalyAnalyzer) Run(stop <-chan struct{}) {
	klog.Infof("Starting anormaly analyzer.")

	// Wait for the caches to be synced before starting workers
	if !cache.WaitForNamedCacheSync("anormaly-analyzer",
		stop,
		s.podSynced,
		s.nodeSynced,
		s.nodeQOSSynced,
		s.avoidanceActionSynced,
	) {
		return
	}

	go func() {
		for {
			select {
			case state := <-s.stateChann:
				start := time.Now()
				metrics.UpdateLastTime(string(known.ModuleAnormalyAnalyzer), metrics.StepMain, start)
				s.Analyze(state)
				metrics.UpdateDurationFromStart(string(known.ModuleAnormalyAnalyzer), metrics.StepMain, start)
			case <-stop:
				klog.Infof("AnormalyAnalyzer exit")
				return
			}
		}
	}()

	return
}

func (s *AnormalyAnalyzer) Analyze(state map[string][]common.TimeSeries) {
	node, err := s.nodeLister.Get(s.nodeName)
	if err != nil {
		klog.Errorf("Failed to get node: %v", err)
		return
	}

	var nodeQOSs []*ensuranceapi.NodeQOS
	allNodeQOSs, err := s.nodeQOSLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list NodeQOS: %v", err)
		return
	}

	for _, nodeQOS := range allNodeQOSs {
		if matched, err := utils.LabelSelectorMatched(node.Labels, nodeQOS.Spec.Selector); err != nil || !matched {
			continue
		}
		nodeQOSs = append(nodeQOSs, nodeQOS.DeepCopy())
	}

	var avoidanceMaps = make(map[string]*ensuranceapi.AvoidanceAction)
	allAvoidance, err := s.avoidanceActionLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list AvoidanceActions, %v", err)
		return
	}

	for _, a := range allAvoidance {
		avoidanceMaps[a.Name] = a
	}

	// step 2: do analyze for nodeQOSs
	var actionContexts []ecache.ActionContext
	for _, n := range nodeQOSs {
		for _, v := range n.Spec.Rules {
			var key = strings.Join([]string{n.Name, v.Name}, ".")
			actionContext, err := s.analyze(key, v, state)
			if err != nil {
				metrics.UpdateAnalyzerWithKeyStatus(metrics.AnalyzeTypeAnalyzeError, key, 1.0)
				klog.Errorf("Failed to analyze, %v.", err)
			}
			metrics.UpdateAnalyzerWithKeyStatus(metrics.AnalyzeTypeAvoidance, key, float64(utils.Bool2Int32(actionContext.Triggered)))
			metrics.UpdateAnalyzerWithKeyStatus(metrics.AnalyzeTypeRestore, key, float64(utils.Bool2Int32(actionContext.Restored)))

			actionContext.NodeQOS = n
			actionContexts = append(actionContexts, actionContext)
		}
	}

	klog.V(6).Infof("Analyze actionContexts: %#v", actionContexts)

	//step 3 : merge
	avoidanceAction := s.merge(state, avoidanceMaps, actionContexts)
	if err != nil {
		klog.Errorf("Failed to merge, %v.", err)
		return
	}

	//step 4 : notice the enforcer manager
	s.notify(avoidanceAction)

	return
}

func (s *AnormalyAnalyzer) getSeries(state []common.TimeSeries, selector *metav1.LabelSelector, metricName string) ([]common.TimeSeries, error) {
	series := s.getTimeSeriesFromMap(state, selector)
	if len(series) == 0 {
		return []common.TimeSeries{}, fmt.Errorf("time series length is 0 for metric %s", metricName)
	}
	return series, nil
}

func (s *AnormalyAnalyzer) trigger(series []common.TimeSeries, object ensuranceapi.Rule) bool {
	var triggered, threshold bool
	for _, ts := range series {
		triggered = s.evaluator.EvalWithMetric(object.MetricRule.Name, float64(object.MetricRule.Value.Value()), ts.Samples[0].Value)

		klog.V(4).Infof("Anormaly detection result %v, Name: %s, Value: %.2f, %s/%s", triggered,
			object.MetricRule.Name,
			ts.Samples[0].Value,
			common.GetValueByName(ts.Labels, common.LabelNamePodNamespace),
			common.GetValueByName(ts.Labels, common.LabelNamePodName))

		if triggered {
			threshold = true
		}
	}
	return threshold
}

func (s *AnormalyAnalyzer) analyze(key string, rule ensuranceapi.Rule, stateMap map[string][]common.TimeSeries) (ecache.ActionContext, error) {
	var actionContext = ecache.ActionContext{Strategy: rule.Strategy, RuleName: rule.Name, ActionName: rule.AvoidanceActionName}

	state, ok := stateMap[rule.MetricRule.Name]
	if !ok {
		return actionContext, fmt.Errorf("metric %s not found", rule.MetricRule.Name)
	}

	//step1: get series from value
	series, err := s.getSeries(state, rule.MetricRule.Selector, rule.MetricRule.Name)
	if err != nil {
		return actionContext, err
	}

	//step2: check if triggered for NodeQOSEnsurance
	threshold := s.trigger(series, rule)
	klog.V(4).Infof("For NodeQOS %s, metrics reach the threshold: %v", key, threshold)

	//step3: check is triggered action or restored, set the detection
	s.computeActionContext(threshold, key, rule, &actionContext)

	return actionContext, nil
}

func (s *AnormalyAnalyzer) computeActionContext(threshold bool, key string, object ensuranceapi.Rule, ac *ecache.ActionContext) {
	if threshold {
		s.restored[key] = 0
		triggered := utils.GetUint64FromMaps(key, s.triggered)
		triggered++
		s.triggered[key] = triggered
		if triggered >= uint64(object.AvoidanceThreshold) {
			ac.Triggered = true
		}
	} else {
		s.triggered[key] = 0
		restored := utils.GetUint64FromMaps(key, s.restored)
		restored++
		s.restored[key] = restored
		if restored >= uint64(object.RestoreThreshold) {
			ac.Restored = true
		}
	}
}

func (s *AnormalyAnalyzer) filterDryRun(acs []ecache.ActionContext) []ecache.ActionContext {
	var dcsFiltered []ecache.ActionContext
	now := time.Now()
	for _, ac := range acs {
		s.logEvent(ac, now)
		if !(ac.Strategy == ensuranceapi.AvoidanceActionStrategyPreview) {
			dcsFiltered = append(dcsFiltered, ac)
		}
	}
	return dcsFiltered
}

func (s *AnormalyAnalyzer) merge(stateMap map[string][]common.TimeSeries, actionMap map[string]*ensuranceapi.AvoidanceAction, actionContexts []ecache.ActionContext) executor.AvoidanceExecutor {
	var executor executor.AvoidanceExecutor

	//step1 filter dry run ActionContext
	filteredActionContext := s.filterDryRun(actionContexts)

	//step2 do DisableScheduled merge
	s.mergeSchedulingActions(filteredActionContext, actionMap, &executor)

	for _, actionCtx := range filteredActionContext {
		action, ok := actionMap[actionCtx.ActionName]
		if !ok {
			klog.Warningf("The action %s not found.", actionCtx.ActionName)
			continue
		}

		//step3 get and deduplicate throttlePods, throttleUpPods
		if action.Spec.Throttle != nil {
			throttlePods, throttleUpPods := s.getThrottlePods(actionCtx, action, stateMap)

			// combine the throttle watermark
			combineThrottleWatermark(&executor.ThrottleExecutor, actionCtx)
			// combine the replicated pod
			combineThrottleDuplicate(&executor.ThrottleExecutor, throttlePods, throttleUpPods)
		}

		//step4 get and deduplicate evictPods
		if action.Spec.Eviction != nil {
			evictPods := s.getEvictPods(actionCtx.Triggered, action, stateMap)

			// combine the evict watermark
			combineEvictWatermark(&executor.EvictExecutor, actionCtx)
			// combine the replicated pod
			combineEvictDuplicate(&executor.EvictExecutor, evictPods)
		}
	}
	executor.StateMap = stateMap

	klog.V(6).Infof("ThrottleExecutor is %#v, EvictExecutor is %#v", executor.ThrottleExecutor, executor.EvictExecutor)

	return executor
}

func (s *AnormalyAnalyzer) logEvent(ac ecache.ActionContext, now time.Time) {
	var key = strings.Join([]string{ac.NodeQOS.Name, ac.RuleName}, "/")

	if !(ac.Triggered || ac.Restored) {
		return
	}

	nodeRef := utils.GetNodeRef(s.nodeName)

	//step1 print log if the detection state is changed
	//step2 produce event
	if ac.Triggered {
		klog.V(4).Infof("LOG: %s triggered action %s", key, ac.ActionName)

		// record an event about the objective ensurance triggered
		s.recorder.Event(nodeRef, v1.EventTypeWarning, "AvoidanceTriggered", fmt.Sprintf("%s triggered action %s", key, ac.ActionName))
		s.actionEventStatus[key] = ecache.DetectionStatus{IsTriggered: true, LastTime: now}
	}

	if ac.Restored {
		if s.actionTriggered(ac) {
			klog.V(4).Infof("LOG: %s restored action %s", key, ac.ActionName)
			// record an event about the objective ensurance restored
			s.recorder.Event(nodeRef, v1.EventTypeNormal, "RestoreTriggered", fmt.Sprintf("%s restored action %s", key, ac.ActionName))
			s.actionEventStatus[key] = ecache.DetectionStatus{IsTriggered: false, LastTime: now}
		}
	}

	return
}

func (s *AnormalyAnalyzer) getTimeSeriesFromMap(state []common.TimeSeries, selector *metav1.LabelSelector) []common.TimeSeries {
	var series []common.TimeSeries

	// step1: get the series from maps
	for _, vv := range state {
		if matched, err := utils.LabelSelectorMatched(common.Labels2Maps(vv.Labels), selector); err != nil {
			continue
		} else if !matched {
			continue
		} else {
			series = append(series, vv)
		}
	}
	return series
}

func (s *AnormalyAnalyzer) notify(as executor.AvoidanceExecutor) {
	//step1: notice by channel
	s.actionCh <- as
	return
}

func (s *AnormalyAnalyzer) actionTriggered(ac ecache.ActionContext) bool {
	var key = strings.Join([]string{ac.NodeQOS.Name, ac.RuleName}, "/")

	if v, ok := s.actionEventStatus[key]; ok {
		if ac.Restored {
			if v.IsTriggered {
				return true
			}
		}
	}

	return false
}

func (s *AnormalyAnalyzer) getThrottlePods(actionCtx ecache.ActionContext, action *ensuranceapi.AvoidanceAction, stateMap map[string][]common.TimeSeries) ([]podinfo.PodContext, []podinfo.PodContext) {

	throttlePods, throttleUpPods := []podinfo.PodContext{}, []podinfo.PodContext{}

	if !actionCtx.Triggered && !actionCtx.Restored {
		return throttlePods, throttleUpPods
	}

	allPods, err := s.podLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list all pods: %v", err)
		return throttlePods, throttleUpPods
	}

	filteredPods, err := s.filterPodQOSMatches(allPods)
	if err != nil {
		klog.Errorf("Failed to filter all pods: %v.", err)
		return throttlePods, throttleUpPods
	}
	for _, pod := range filteredPods {
		if actionCtx.Triggered {
			throttlePods = append(throttlePods, podinfo.BuildPodActionContext(pod, stateMap, action, podinfo.ThrottleDown))
		}
		if actionCtx.Restored {
			throttleUpPods = append(throttleUpPods, podinfo.BuildPodActionContext(pod, stateMap, action, podinfo.ThrottleUp))
		}
	}

	return throttlePods, throttleUpPods
}

func (s *AnormalyAnalyzer) getEvictPods(triggered bool, action *ensuranceapi.AvoidanceAction, stateMap map[string][]common.TimeSeries) []podinfo.PodContext {
	evictPods := []podinfo.PodContext{}

	if triggered {
		allPods, err := s.podLister.List(labels.Everything())
		if err != nil {
			klog.Errorf("Failed to list all pods: %v.", err)
			return evictPods
		}
		filteredPods, err := s.filterPodQOSMatches(allPods)
		if err != nil {
			klog.Errorf("Failed to filter all pods: %v.", err)
			return evictPods
		}
		for _, pod := range filteredPods {
			evictPods = append(evictPods, podinfo.BuildPodActionContext(pod, stateMap, action, podinfo.Evict))

		}
	}
	return evictPods
}

func (s *AnormalyAnalyzer) filterPodQOSMatches(pods []*v1.Pod) ([]*v1.Pod, error) {
	filteredPods := []*v1.Pod{}
	podQOSList, err := s.podQOSLister.List(labels.Everything())
	// todo: not found error should be ignored
	if err != nil {
		klog.Errorf("Failed to list NodeQOS: %v", err)
		return filteredPods, err
	}
	for _, qos := range podQOSList {
		for _, pod := range pods {
			if match(pod, qos) {
				filteredPods = append(filteredPods, pod)
			}
		}
	}
	return filteredPods, nil
}

func (s *AnormalyAnalyzer) mergeSchedulingActions(actionContexts []ecache.ActionContext, avoidanceMaps map[string]*ensuranceapi.AvoidanceAction, ae *executor.AvoidanceExecutor) {
	var now = time.Now()

	// If the ensurance rules are empty, it must be recovered soon.
	// So we set enableScheduling true
	if len(actionContexts) == 0 {
		s.ToggleScheduleSetting(ae, false)
	} else {
		for _, ac := range actionContexts {
			action, ok := avoidanceMaps[ac.ActionName]
			if !ok {
				klog.Warningf("Action %s defined in nodeQOS %s is not found", ac.ActionName, ac.NodeQOS.Name)
				continue
			}

			if ac.Triggered {
				metrics.UpdateAnalyzerStatus(metrics.AnalyzeTypeEnableScheduling, float64(0))
				s.ToggleScheduleSetting(ae, true)
			}

			if ac.Restored {
				if now.After(s.lastTriggeredTime.Add(time.Duration(action.Spec.CoolDownSeconds) * time.Second)) {
					metrics.UpdateAnalyzerStatus(metrics.AnalyzeTypeEnableScheduling, float64(1))
					s.ToggleScheduleSetting(ae, false)
				}
			}
		}
	}
}

func (s *AnormalyAnalyzer) ToggleScheduleSetting(ae *executor.AvoidanceExecutor, toBeDisable bool) {
	if toBeDisable {
		s.lastTriggeredTime = time.Now()
	}
	ae.ScheduleExecutor.ToBeDisable = toBeDisable
	ae.ScheduleExecutor.ToBeRestore = !ae.ScheduleExecutor.ToBeDisable
}

func combineThrottleDuplicate(e *executor.ThrottleExecutor, throttlePods, throttleUpPods executor.ThrottlePods) {
	for _, t := range throttlePods {
		if i := e.ThrottleDownPods.Find(t.Key); i == -1 {
			e.ThrottleDownPods = append(e.ThrottleDownPods, t)
		} else {
			if t.CPUThrottle.MinCPURatio > e.ThrottleDownPods[i].CPUThrottle.MinCPURatio {
				e.ThrottleDownPods[i].CPUThrottle.MinCPURatio = t.CPUThrottle.MinCPURatio
			}

			if t.CPUThrottle.StepCPURatio > e.ThrottleDownPods[i].CPUThrottle.StepCPURatio {
				e.ThrottleDownPods[i].CPUThrottle.StepCPURatio = t.CPUThrottle.StepCPURatio
			}
		}
	}

	for _, t := range throttleUpPods {
		if i := e.ThrottleUpPods.Find(t.Key); i == -1 {
			e.ThrottleUpPods = append(e.ThrottleUpPods, t)
		} else {
			if t.CPUThrottle.MinCPURatio > e.ThrottleUpPods[i].CPUThrottle.MinCPURatio {
				e.ThrottleUpPods[i].CPUThrottle.MinCPURatio = t.CPUThrottle.MinCPURatio
			}

			if t.CPUThrottle.StepCPURatio > e.ThrottleUpPods[i].CPUThrottle.StepCPURatio {
				e.ThrottleUpPods[i].CPUThrottle.StepCPURatio = t.CPUThrottle.StepCPURatio
			}
		}
	}
}

func combineEvictDuplicate(e *executor.EvictExecutor, evictPods executor.EvictPods) {
	for _, ep := range evictPods {
		if i := e.EvictPods.Find(ep.Key); i == -1 {
			e.EvictPods = append(e.EvictPods, ep)
		} else {
			if (ep.DeletionGracePeriodSeconds != nil) && ((e.EvictPods[i].DeletionGracePeriodSeconds == nil) ||
				(*(e.EvictPods[i].DeletionGracePeriodSeconds) > *(ep.DeletionGracePeriodSeconds))) {
				e.EvictPods[i].DeletionGracePeriodSeconds = ep.DeletionGracePeriodSeconds
			}
		}
	}
}

func combineThrottleWatermark(e *executor.ThrottleExecutor, ac ecache.ActionContext) {
	if !ac.Triggered && !ac.Restored {
		return
	}

	if ac.Triggered {
		for _, ensurance := range ac.NodeQOS.Spec.Rules {
			if ensurance.Name == ac.RuleName {
				if e.ThrottleDownWatermark == nil {
					e.ThrottleDownWatermark = make(map[executor.WatermarkMetric]*executor.Watermark)
				}
				// Use a heap here, so we don't need to use <nodeQOSName>-<MetricRuleName> as value, just use <MetricRuleName>
				if e.ThrottleDownWatermark[executor.WatermarkMetric(ensurance.MetricRule.Name)] == nil {
					e.ThrottleDownWatermark[executor.WatermarkMetric(ensurance.MetricRule.Name)] = &executor.Watermark{}
				}
				heap.Push(e.ThrottleDownWatermark[executor.WatermarkMetric(ensurance.MetricRule.Name)], ensurance.MetricRule.Value)
			}
		}
	}

	for watermarkMetric, watermarks := range e.ThrottleDownWatermark {
		klog.V(6).Infof("ThrottleDownWatermark info: metric: %s, value: %#v", watermarkMetric, watermarks)
	}

	if ac.Restored {
		for _, ensurance := range ac.NodeQOS.Spec.Rules {
			if ensurance.Name == ac.RuleName {
				if e.ThrottleUpWatermark == nil {
					e.ThrottleUpWatermark = make(map[executor.WatermarkMetric]*executor.Watermark)
				}
				if e.ThrottleUpWatermark[executor.WatermarkMetric(ensurance.MetricRule.Name)] == nil {
					e.ThrottleUpWatermark[executor.WatermarkMetric(ensurance.MetricRule.Name)] = &executor.Watermark{}
				}
				heap.Push(e.ThrottleUpWatermark[executor.WatermarkMetric(ensurance.MetricRule.Name)], ensurance.MetricRule.Value)
			}
		}
	}

	for watermarkMetric, watermarks := range e.ThrottleUpWatermark {
		klog.V(6).Infof("ThrottleUpWatermark info: metric: %s, value: %#v", watermarkMetric, watermarks)
	}
}

func combineEvictWatermark(e *executor.EvictExecutor, ac ecache.ActionContext) {
	if !ac.Triggered {
		return
	}

	for _, ensurance := range ac.NodeQOS.Spec.Rules {
		if ensurance.Name == ac.RuleName {
			if e.EvictWatermark == nil {
				e.EvictWatermark = make(map[executor.WatermarkMetric]*executor.Watermark)
			}
			if e.EvictWatermark[executor.WatermarkMetric(ensurance.MetricRule.Name)] == nil {
				e.EvictWatermark[executor.WatermarkMetric(ensurance.MetricRule.Name)] = &executor.Watermark{}
			}
			heap.Push(e.EvictWatermark[executor.WatermarkMetric(ensurance.MetricRule.Name)], ensurance.MetricRule.Value)
		}
	}

	for watermarkMetric, watermarks := range e.EvictWatermark {
		klog.V(6).Infof("EvictWatermark info: metric: %s, value: %#v", watermarkMetric, watermarks)
	}
}
