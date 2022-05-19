package executor

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	podinfo "github.com/gocrane/crane/pkg/ensurance/executor/pod-info"
	execsort "github.com/gocrane/crane/pkg/ensurance/executor/sort"
	"github.com/gocrane/crane/pkg/known"
	"github.com/gocrane/crane/pkg/metrics"
	"github.com/gocrane/crane/pkg/utils"
)

type EvictExecutor struct {
	EvictPods EvictPods
	// All metrics(not only can be quantified metrics) metioned in triggerd NodeQOSEnsurancePolicy and their corresponding waterlines
	EvictWaterLine WaterLines
}

type EvictPods []podinfo.PodContext

func (e EvictPods) Find(key types.NamespacedName) int {
	for i, v := range e {
		if v.PodKey == key {
			return i
		}
	}

	return -1
}

func (e *EvictExecutor) Avoid(ctx *ExecuteContext) error {
	var start = time.Now()
	metrics.UpdateLastTimeWithSubComponent(string(known.ModuleActionExecutor), string(metrics.SubComponentEvict), metrics.StepAvoid, start)
	defer metrics.UpdateDurationFromStartWithSubComponent(string(known.ModuleActionExecutor), string(metrics.SubComponentEvict), metrics.StepAvoid, start)

	klog.V(6).Infof("EvictExecutor avoid, %v", *e)

	if len(e.EvictPods) == 0 {
		metrics.UpdateExecutorStatus(metrics.SubComponentEvict, metrics.StepAvoid, 0.0)
		return nil
	}

	metrics.UpdateExecutorStatus(metrics.SubComponentEvict, metrics.StepAvoid, 1.0)
	metrics.ExecutorStatusCounterInc(metrics.SubComponentEvict, metrics.StepAvoid)

	var errPodKeys, errKeys []string
	// TODO: totalReleasedResource used for prom metrics
	var totalReleased ReleaseResource

	/* The step to evict:
	1.1 If EvictWaterLine has metrics that not in WaterLineMetricsCanBeQualified, evict all evictPods and calculate the release resource, then return
	1.2 Or if a metric which both in WaterLineMetricsCanBeQualified and EvictWaterLine doesn't has usage value in statemap, which means we can't get the
	   accurate value from collector, then evict all evictPods and return
	2. If there is gap to the metrics in WaterLineMetricsCanBeQualified, evict finely according to the gap
		3.1 First sort pods by memory metrics, because it is incompressible more urgent; Then evict sorted pods one by one util there is
	        no gap to waterline on memory usage
		3.2 Then sort pods by cpu metrics, Then evict sorted pods one by one util there is no gap to waterline on cpu usage
	*/

	// If there is a metric that can't be qualified or If there is metric in EvictGapToWaterLines(get from trigger NodeQOSEnsurancePolicy) that can't get current usage,
	//then evict all selected evictedPod and return
	if e.EvictWaterLine.HasMetricNotInCanbeQualified(EvictMetricsCanBeQualified) || ctx.EvictGapToWaterLines.HasUsageMissedMetric() {
		errPodKeys = e.evictPods(ctx, &totalReleased)
	} else {
		_, _, ctx.EvictGapToWaterLines = buildGapToWaterLine(ctx.getStateFunc(), ThrottleExecutor{}, *e)
		// The metrics in EvictGapToWaterLines are all in WaterLineMetricsCanBeQualified and they has current usage,we can do evict precisely
		wg := sync.WaitGroup{}
		var released ReleaseResource

		// First evict pods according to incompressible resource: memory
		execsort.MemMetricsSorter(e.EvictPods)
		for !ctx.EvictGapToWaterLines.TargetGapsRemoved(MemUsage) {
			if podinfo.HasNoExecutedPod(e.EvictPods) {
				index := podinfo.GetFirstNoExecutedPod(e.EvictPods)
				errKeys, released = e.evictOnePod(&wg, ctx, index, &totalReleased)
				errPodKeys = append(errPodKeys, errKeys...)
				ctx.EvictGapToWaterLines[string(MemUsage)] -= released.MemUsage
				ctx.EvictGapToWaterLines[string(CpuUsage)] -= released.CpuUsage
			}
		}
		// Then evict pods according to compressible resource: cpu
		execsort.CpuMetricsSorter(e.EvictPods)
		for !ctx.EvictGapToWaterLines.TargetGapsRemoved(CpuUsage) {
			if podinfo.HasNoExecutedPod(e.EvictPods) {
				index := podinfo.GetFirstNoExecutedPod(e.EvictPods)
				errKeys, released = e.evictOnePod(&wg, ctx, index, &totalReleased)
				errPodKeys = append(errPodKeys, errKeys...)
				ctx.EvictGapToWaterLines[string(MemUsage)] -= released.MemUsage
				ctx.EvictGapToWaterLines[string(CpuUsage)] -= released.CpuUsage
			}
		}
		wg.Wait()
	}

	if len(errPodKeys) != 0 {
		return fmt.Errorf("some pod evict failed,err: %s", strings.Join(errPodKeys, ";"))
	}

	return nil
}

func (e *EvictExecutor) Restore(ctx *ExecuteContext) error {
	return nil
}

func (e *EvictExecutor) evictPods(ctx *ExecuteContext, totalReleasedResource *ReleaseResource) (errPodKeys []string) {
	wg := sync.WaitGroup{}
	for i := range e.EvictPods {
		errKeys, _ := e.evictOnePod(&wg, ctx, i, totalReleasedResource)
		errPodKeys = append(errPodKeys, errKeys...)
	}
	wg.Wait()
	return
}

func (e *EvictExecutor) evictOnePod(wg *sync.WaitGroup, ctx *ExecuteContext, index int, totalReleasedResource *ReleaseResource) (errPodKeys []string, released ReleaseResource) {
	wg.Add(1)

	go func(evictPod podinfo.PodContext) {
		defer wg.Done()

		pod, err := ctx.PodLister.Pods(evictPod.PodKey.Namespace).Get(evictPod.PodKey.Name)
		if err != nil {
			errPodKeys = append(errPodKeys, "not found ", evictPod.PodKey.String())
			return
		}

		err = utils.EvictPodWithGracePeriod(ctx.Client, pod, evictPod.DeletionGracePeriodSeconds)
		if err != nil {
			errPodKeys = append(errPodKeys, "evict failed ", evictPod.PodKey.String())
			klog.Warningf("Failed to evict pod %s: %v", evictPod.PodKey.String(), err)
			return
		}

		metrics.ExecutorEvictCountsInc()

		klog.V(4).Infof("Pod %s is evicted", klog.KObj(pod))

		// Calculate release resources
		released = ConstructRelease(evictPod, 0.0, 0.0)
		totalReleasedResource.Add(released)
	}(e.EvictPods[index])
	return
}
