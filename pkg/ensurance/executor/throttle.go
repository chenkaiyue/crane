package executor

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	podinfo "github.com/gocrane/crane/pkg/ensurance/executor/pod-info"
	execsort "github.com/gocrane/crane/pkg/ensurance/executor/sort"
	"github.com/gocrane/crane/pkg/known"
	"github.com/gocrane/crane/pkg/metrics"
)

const (
	MaxUpQuota          = 60 * 1000 // 60CU
	CpuQuotaCoefficient = 1000.0
	MaxRatio            = 100.0
)

type ThrottleExecutor struct {
	ThrottleDownPods ThrottlePods
	ThrottleUpPods   ThrottlePods
	// All metrics(not only metrics that can be quantified) metioned in triggerd NodeQOSEnsurancePolicy and their corresponding waterlines
	ThrottleDownWaterLine WaterLines
	ThrottleUpWaterLine   WaterLines
}

type ThrottlePods []podinfo.PodContext

func (t ThrottlePods) Find(podTypes types.NamespacedName) int {
	for i, v := range t {
		if v.PodKey == podTypes {
			return i
		}
	}

	return -1
}

func Reverse(t ThrottlePods) []podinfo.PodContext {
	throttle := []podinfo.PodContext(t)
	l := len(throttle)
	for i := 0; i < l/2; i++ {
		throttle[i], throttle[l-i-1] = throttle[l-i-1], throttle[i]
	}
	return throttle
}

func (t *ThrottleExecutor) Avoid(ctx *ExecuteContext) error {
	var start = time.Now()
	metrics.UpdateLastTimeWithSubComponent(string(known.ModuleActionExecutor), string(metrics.SubComponentThrottle), metrics.StepAvoid, start)
	defer metrics.UpdateDurationFromStartWithSubComponent(string(known.ModuleActionExecutor), string(metrics.SubComponentThrottle), metrics.StepAvoid, start)

	klog.V(6).Info("ThrottleExecutor avoid, %v", *t)

	if len(t.ThrottleDownPods) == 0 {
		metrics.UpdateExecutorStatus(metrics.SubComponentThrottle, metrics.StepAvoid, 0)
	} else {
		metrics.UpdateExecutorStatus(metrics.SubComponentThrottle, metrics.StepAvoid, 1.0)
		metrics.ExecutorStatusCounterInc(metrics.SubComponentThrottle, metrics.StepAvoid)
	}

	var errPodKeys, errKeys []string
	// TODO: totalReleasedResource used for prom metrics
	var totalReleased ReleaseResource

	/* The step to throttle:
	1. If ThrottleDownWaterLine has metrics that not in WaterLineMetricsCanBeQualified, throttle all ThrottleDownPods and calculate the release resource, then return
	2. If a metric which both in WaterLineMetricsCanBeQualified and ThrottleDownWaterLine doesn't has usage value in statemap, which means we can't get the
	   accurate value from collector, then throttle all ThrottleDownPods and return
	3. If there is gap to the metrics in WaterLineMetricsCanBeQualified, throttle finely according to the gap
		3.1 First sort pods by memory metrics, because it is incompressible more urgent; Then throttle sorted pods one by one util there is
	        no gap to waterline on memory usage
		3.2 Then sort pods by cpu metrics, Then throttle sorted pods one by one util there is no gap to waterline on cpu usage
	*/
	metricsThrottleQualified, MetricsNotThrottleQualified := t.ThrottleDownWaterLine.DivideMetricsByThrottleQualified()

	// There is a metric that can't be ThrottleQualified, so throttle all selected pods
	if len(MetricsNotThrottleQualified) != 0 {
		throttleAbleMetrics := t.ThrottleDownWaterLine.GetMetricsThrottleAble()
		if len(throttleAbleMetrics) != 0{
			errPodKeys = t.throttlePods(ctx, &totalReleased, throttleAbleMetrics[0])
		}
	} else {
		ctx.ThrottoleDownGapToWaterLines, _, _ = buildGapToWaterLine(ctx.getStateFunc(), *t, EvictExecutor{})
		// The metrics in ThrottoleDownGapToWaterLines are all in WaterLineMetricsCanBeQualified and has current usage, then throttle precisely
		var released ReleaseResource
		for _, m := range metricsThrottleQualified {
			if MetricMap[m].SortAble {
				MetricMap[m].SortFunc(t.ThrottleDownPods)
			} else {
				execsort.GeneralSorter(t.ThrottleDownPods)
			}

			for !ctx.ThrottoleDownGapToWaterLines.TargetGapsRemoved(m) {
				for index, _ := range t.ThrottleDownPods {
					errKeys, released = MetricMap[m].ThrottleFunc(ctx, index, t.ThrottleDownPods, &totalReleased)
					errPodKeys = append(errPodKeys, errKeys...)
					ctx.ThrottoleDownGapToWaterLines[m] -= released[m]
				}
			}
		}
	}

	if len(errPodKeys) != 0 {
		return fmt.Errorf("some pod throttle failed,err: %s", strings.Join(errPodKeys, ";"))
	}

	return nil
}

func (t *ThrottleExecutor) throttlePods(ctx *ExecuteContext, totalReleasedResource *ReleaseResource, m WaterLineMetric) (errPodKeys []string) {
	for i := range t.ThrottleDownPods {
		errKeys, _ := MetricMap[m].ThrottleFunc(ctx, i, t.ThrottleDownPods, totalReleasedResource)
		errPodKeys = append(errPodKeys, errKeys...)
	}
	return
}

func (t *ThrottleExecutor) Restore(ctx *ExecuteContext) error {
	var start = time.Now()
	metrics.UpdateLastTimeWithSubComponent(string(known.ModuleActionExecutor), string(metrics.SubComponentThrottle), metrics.StepRestore, start)
	defer metrics.UpdateDurationFromStartWithSubComponent(string(known.ModuleActionExecutor), string(metrics.SubComponentThrottle), metrics.StepRestore, start)

	klog.V(6).Info("ThrottleExecutor restore, %v", *t)

	if len(t.ThrottleUpPods) == 0 {
		metrics.UpdateExecutorStatus(metrics.SubComponentThrottle, metrics.StepRestore, 0)
		return nil
	}

	metrics.UpdateExecutorStatus(metrics.SubComponentThrottle, metrics.StepRestore, 1.0)
	metrics.ExecutorStatusCounterInc(metrics.SubComponentThrottle, metrics.StepRestore)

	var errPodKeys, errKeys []string
	// TODO: totalReleasedResource used for prom metrics
	var totalReleased ReleaseResource

	/* The step to restore:
	1. If ThrottleUpWaterLine has metrics that not in WaterLineMetricsCanBeQualified, restore all ThrottleUpPods and calculate the difference of resource, then return
	2. If a metric which both in WaterLineMetricsCanBeQualified and ThrottleUpWaterLine doesn't has usage value in statemap, which means we can't get the
	   accurate value from collector, then restore all ThrottleUpPods and return
	3. If there is gap to the metrics in WaterLineMetricsCanBeQualified, restore finely according to the gap
		3.1 First sort pods by memory metrics, because it is incompressible more urgent; Then restore sorted pods one by one util there is
	        no gap to waterline on memory usage
		3.2 Then sort pods by cpu metrics, Then restore sorted pods one by one util there is no gap to waterline on cpu usage
	*/

	metricsThrottleQualified, MetricsNotThrottleQualified := t.ThrottleUpWaterLine.DivideMetricsByThrottleQualified()

	// There is a metric that can't be ThrottleQualified, so throttle all selected pods
	if len(MetricsNotThrottleQualified) != 0 {
		errPodKeys = t.restorePods(ctx, &totalReleased, MetricsNotThrottleQualified[0])
	} else {
		_, ctx.ThrottoleUpGapToWaterLines, _ = buildGapToWaterLine(ctx.getStateFunc(), *t, EvictExecutor{})
		// The metrics in ThrottoleUpGapToWaterLines are all in WaterLineMetricsCanBeQualified and has current usage, then throttle precisely
		var released ReleaseResource
		for _, m := range metricsThrottleQualified {
			if MetricMap[m].SortAble {
				MetricMap[m].SortFunc(t.ThrottleUpPods)
			} else {
				execsort.GeneralSorter(t.ThrottleUpPods)
			}
			t.ThrottleUpPods = Reverse(t.ThrottleUpPods)

			for !ctx.ThrottoleUpGapToWaterLines.TargetGapsRemoved(m) {
				for index, _ := range t.ThrottleUpPods {
					errKeys, released = MetricMap[m].RestoreFunc(ctx, index, t.ThrottleUpPods, &totalReleased)
					errPodKeys = append(errPodKeys, errKeys...)
					ctx.ThrottoleUpGapToWaterLines[m] -= released[m]
				}
			}
		}
	}

	if len(errPodKeys) != 0 {
		return fmt.Errorf("some pod throttle restore failed,err: %s", strings.Join(errPodKeys, ";"))
	}

	return nil
}

func (t *ThrottleExecutor) restorePods(ctx *ExecuteContext, totalReleasedResource *ReleaseResource, m WaterLineMetric) (errPodKeys []string) {
	for i := range t.ThrottleUpPods {
		errKeys, _ := MetricMap[m].RestoreFunc(ctx, i, t.ThrottleDownPods, totalReleasedResource)
		errPodKeys = append(errPodKeys, errKeys...)
	}
	return
}
