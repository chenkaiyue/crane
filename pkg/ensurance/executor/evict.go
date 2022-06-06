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

	metricsEvictQualified, MetricsNotEvcitQualified := e.EvictWaterLine.DivideMetricsByEvictQualified()

	// There is a metric that can't be ThrottleQualified, so throttle all selected pods
	if len(MetricsNotEvcitQualified) != 0 {
		evictAbleMetrics := e.EvictWaterLine.GetMetricsEvictAble()
		if len(evictAbleMetrics) != 0 {
			errPodKeys = e.evictPods(ctx, &totalReleased, evictAbleMetrics[0])
		}
	} else {
		_, _, ctx.EvictGapToWaterLines = buildGapToWaterLine(ctx.getStateFunc(), ThrottleExecutor{}, *e)
		// The metrics in ThrottoleDownGapToWaterLines are all in WaterLineMetricsCanBeQualified and has current usage, then throttle precisely
		wg := sync.WaitGroup{}
		var released ReleaseResource
		for _, m := range metricsEvictQualified {
			if MetricMap[m].SortAble {
				MetricMap[m].SortFunc(e.EvictPods)
			} else {
				execsort.GeneralSorter(e.EvictPods)
			}

			for !ctx.EvictGapToWaterLines.TargetGapsRemoved(m) {
				if podinfo.HasNoExecutedPod(e.EvictPods) {
					index := podinfo.GetFirstNoExecutedPod(e.EvictPods)
					errKeys, released = MetricMap[m].EvictFunc(&wg, ctx, index, &totalReleased, e.EvictPods)
					errPodKeys = append(errPodKeys, errKeys...)

					e.EvictPods[index].HasBeenActioned = true
					ctx.EvictGapToWaterLines[m] -= released[m]
				}
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

func (e *EvictExecutor) evictPods(ctx *ExecuteContext, totalReleasedResource *ReleaseResource, m WaterLineMetric) (errPodKeys []string) {
	wg := sync.WaitGroup{}
	for i := range e.EvictPods {
		errKeys, _ := MetricMap[m].EvictFunc(&wg, ctx, i, totalReleasedResource, e.EvictPods)
		errPodKeys = append(errPodKeys, errKeys...)
	}
	wg.Wait()
	return
}
