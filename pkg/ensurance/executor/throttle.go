package executor

import (
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	podinfo "github.com/gocrane/crane/pkg/ensurance/executor/pod-info"
	execsort "github.com/gocrane/crane/pkg/ensurance/executor/sort"
	cruntime "github.com/gocrane/crane/pkg/ensurance/runtime"
	"github.com/gocrane/crane/pkg/known"
	"github.com/gocrane/crane/pkg/metrics"
	"github.com/gocrane/crane/pkg/utils"
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

	hasMetricThrottleAble := t.ThrottleDownWaterLine.HasMetricNotInCanbeQualified

	/* The step to throttle:
	1. If ThrottleDownWaterLine has metrics that not in WaterLineMetricsCanBeQualified, throttle all ThrottleDownPods and calculate the release resource, then return
	2. If a metric which both in WaterLineMetricsCanBeQualified and ThrottleDownWaterLine doesn't has usage value in statemap, which means we can't get the
	   accurate value from collector, then throttle all ThrottleDownPods and return
	3. If there is gap to the metrics in WaterLineMetricsCanBeQualified, throttle finely according to the gap
		3.1 First sort pods by memory metrics, because it is incompressible more urgent; Then throttle sorted pods one by one util there is
	        no gap to waterline on memory usage
		3.2 Then sort pods by cpu metrics, Then throttle sorted pods one by one util there is no gap to waterline on cpu usage
	*/

	// If there is a metric that can't be qualified, then we throttle all selected throttlePods and return
	if t.ThrottleDownWaterLine.HasMetricNotInCanbeQualified(ThrottleMetricsCanBeQualified) {
		errPodKeys = t.throttlePods(ctx, &totalReleased)
	} else {
		ctx.ThrottoleDownGapToWaterLines, _, _ = buildGapToWaterLine(ctx.getStateFunc(), *t, EvictExecutor{})
		if ctx.ThrottoleDownGapToWaterLines.HasUsageMissedMetric() {
			// If there is metric in ThrottoleDownGapToWaterLines(get from trigger NodeQOSEnsurancePolicy) that can't get current usage,
			// we have to throttle all selected ThrottleDownPods
			errPodKeys = t.throttlePods(ctx, &totalReleased)
		} else {
			// The metrics in ThrottoleDownGapToWaterLines are all in WaterLineMetricsCanBeQualified and has current usage, then throttle precisely
			var released ReleaseResource

			// First throttle pods according to incompressible resource: memory
			execsort.MemMetricsSorter(t.ThrottleDownPods)
			for !ctx.ThrottoleDownGapToWaterLines.TargetGapsRemoved(MemUsage) {
				if podinfo.HasNoExecutedPod(t.ThrottleDownPods) {
					index := podinfo.GetFirstNoExecutedPod(t.ThrottleDownPods)
					errKeys, released = t.throttleOnePod(ctx, index, &totalReleased)
					errPodKeys = append(errPodKeys, errKeys...)
					ctx.ThrottoleDownGapToWaterLines[string(MemUsage)] -= released.MemUsage
					ctx.ThrottoleDownGapToWaterLines[string(CpuUsage)] -= released.CpuUsage
				}
			}
			// Then throttle pods according to compressible resource: cpu
			execsort.CpuMetricsSorter(t.ThrottleDownPods)
			for !ctx.ThrottoleDownGapToWaterLines.TargetGapsRemoved(CpuUsage) {
				if podinfo.HasNoExecutedPod(t.ThrottleDownPods) {
					index := podinfo.GetFirstNoExecutedPod(t.ThrottleDownPods)
					errKeys, released = t.throttleOnePod(ctx, index, &totalReleased)
					errPodKeys = append(errPodKeys, errKeys...)
					ctx.ThrottoleDownGapToWaterLines[string(MemUsage)] -= released.MemUsage
					ctx.ThrottoleDownGapToWaterLines[string(CpuUsage)] -= released.CpuUsage
				}
			}
		}
	}

	if len(errPodKeys) != 0 {
		return fmt.Errorf("some pod throttle failed,err: %s", strings.Join(errPodKeys, ";"))
	}

	return nil
}

func (t *ThrottleExecutor) throttlePods(ctx *ExecuteContext, totalReleasedResource *ReleaseResource) (errPodKeys []string) {
	for i := range t.ThrottleDownPods {
		errKeys, _ := t.throttleOnePod(ctx, i, totalReleasedResource)
		errPodKeys = append(errPodKeys, errKeys...)
	}
	return
}

func (t *ThrottleExecutor) throttleOnePod(ctx *ExecuteContext, index int, totalReleasedResource *ReleaseResource) (errPodKeys []string, released ReleaseResource) {
	pod, err := ctx.PodLister.Pods(t.ThrottleDownPods[index].PodKey.Namespace).Get(t.ThrottleDownPods[index].PodKey.Name)
	if err != nil {
		errPodKeys = append(errPodKeys, fmt.Sprintf("pod %s not found", t.ThrottleDownPods[index].PodKey.String()))
		return
	}

	// Throttle for CPU metrics

	for _, v := range t.ThrottleDownPods[index].ContainerCPUUsages {
		// pause container to skip
		if v.ContainerName == "" {
			continue
		}

		klog.V(4).Infof("ThrottleExecutor1 avoid container %s/%s", klog.KObj(pod), v.ContainerName)

		containerCPUQuota, err := podinfo.GetUsageById(t.ThrottleDownPods[index].ContainerCPUQuotas, v.ContainerId)
		if err != nil {
			errPodKeys = append(errPodKeys, err.Error(), t.ThrottleDownPods[index].PodKey.String())
			continue
		}

		containerCPUPeriod, err := podinfo.GetUsageById(t.ThrottleDownPods[index].ContainerCPUPeriods, v.ContainerId)
		if err != nil {
			errPodKeys = append(errPodKeys, err.Error(), t.ThrottleDownPods[index].PodKey.String())
			continue
		}

		container, err := utils.GetPodContainerByName(pod, v.ContainerName)
		if err != nil {
			errPodKeys = append(errPodKeys, err.Error(), t.ThrottleDownPods[index].PodKey.String())
			continue
		}

		var containerCPUQuotaNew float64
		if utils.AlmostEqual(containerCPUQuota.Value, -1.0) || utils.AlmostEqual(containerCPUQuota.Value, 0.0) {
			containerCPUQuotaNew = v.Value * (1.0 - float64(t.ThrottleDownPods[index].CPUThrottle.StepCPURatio)/MaxRatio)
		} else {
			containerCPUQuotaNew = containerCPUQuota.Value / containerCPUPeriod.Value * (1.0 - float64(t.ThrottleDownPods[index].CPUThrottle.StepCPURatio)/MaxRatio)
		}

		if requestCPU, ok := container.Resources.Requests[v1.ResourceCPU]; ok {
			if float64(requestCPU.MilliValue())/CpuQuotaCoefficient > containerCPUQuotaNew {
				containerCPUQuotaNew = float64(requestCPU.MilliValue()) / CpuQuotaCoefficient
			}
		}

		if limitCPU, ok := container.Resources.Limits[v1.ResourceCPU]; ok {
			if float64(limitCPU.MilliValue())/CpuQuotaCoefficient*float64(t.ThrottleDownPods[index].CPUThrottle.MinCPURatio)/MaxRatio > containerCPUQuotaNew {
				containerCPUQuotaNew = float64(limitCPU.MilliValue()) * float64(t.ThrottleDownPods[index].CPUThrottle.MinCPURatio) / CpuQuotaCoefficient
			}
		}

		klog.V(6).Infof("Prior update container resources containerCPUQuotaNew %.2f, containerCPUQuota.Value %.2f,containerCPUPeriod %.2f",
			containerCPUQuotaNew, containerCPUQuota.Value, containerCPUPeriod.Value)

		if !utils.AlmostEqual(containerCPUQuotaNew*containerCPUPeriod.Value, containerCPUQuota.Value) {
			err = cruntime.UpdateContainerResources(ctx.RuntimeClient, v.ContainerId, cruntime.UpdateOptions{CPUQuota: int64(containerCPUQuotaNew * containerCPUPeriod.Value)})
			if err != nil {
				errPodKeys = append(errPodKeys, fmt.Sprintf("failed to updateResource for %s/%s, error: %v", t.ThrottleDownPods[index].PodKey.String(), v.ContainerName, err))
				continue
			} else {
				klog.V(4).Infof("ThrottleExecutor avoid pod %s, container %s, set cpu quota %.2f.",
					klog.KObj(pod), v.ContainerName, containerCPUQuotaNew)
			}
		}
		released = ConstructRelease(t.ThrottleDownPods[index], containerCPUQuotaNew, v.Value)
		totalReleasedResource.Add(released)
	}
	return

	//TODO: Throttle Others，such as memroy...

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

	// If there is a metric that can't be qualified, then we do evict for all selected evictedPod and return, because all evictedPod has been evicted
	if t.ThrottleUpWaterLine.HasMetricNotInCanbeQualified(ThrottleMetricsCanBeQualified) {
		errPodKeys = t.restorePods(ctx, &totalReleased)
	} else {
		_, ctx.ThrottoleUpGapToWaterLines, _ = buildGapToWaterLine(ctx.getStateFunc(), *t, EvictExecutor{})
		if ctx.ThrottoleUpGapToWaterLines.HasUsageMissedMetric() {
			// If there is metric in ThrottoleUpGapToWaterLines(get from trigger NodeQOSEnsurancePolicy) that can't get current usage,
			// we have to do restore for all selected ThrottleUpPods
			errPodKeys = t.restorePods(ctx, &totalReleased)
		} else {
			// The metrics in ThrottoleUpGapToWaterLines are all in WaterLineMetricsCanBeQualified and has current usage,we can do restore precisely
			var released ReleaseResource

			// We first restore for incompressible resource: memory
			execsort.MemMetricsSorter(t.ThrottleUpPods)
			t.ThrottleUpPods = Reverse(t.ThrottleUpPods)
			for !ctx.ThrottoleUpGapToWaterLines.TargetGapsRemoved(MemUsage) {
				if podinfo.HasNoExecutedPod(t.ThrottleUpPods) {
					index := podinfo.GetFirstNoExecutedPod(t.ThrottleUpPods)
					errKeys, released = t.restoreOnePod(ctx, index, &totalReleased)
					errPodKeys = append(errPodKeys, errKeys...)
					ctx.ThrottoleUpGapToWaterLines[string(MemUsage)] -= released.MemUsage
					ctx.ThrottoleUpGapToWaterLines[string(CpuUsage)] -= released.CpuUsage
				}
			}
			// Then restore for compressible resource: cpu
			execsort.CpuMetricsSorter(t.ThrottleUpPods)
			t.ThrottleUpPods = Reverse(t.ThrottleUpPods)
			for !ctx.ThrottoleUpGapToWaterLines.TargetGapsRemoved(CpuUsage) {
				if podinfo.HasNoExecutedPod(t.ThrottleUpPods) {
					index := podinfo.GetFirstNoExecutedPod(t.ThrottleUpPods)
					errKeys, released = t.restoreOnePod(ctx, index, &totalReleased)
					errPodKeys = append(errPodKeys, errKeys...)
					ctx.ThrottoleUpGapToWaterLines[string(MemUsage)] -= released.MemUsage
					ctx.ThrottoleUpGapToWaterLines[string(CpuUsage)] -= released.CpuUsage
				}
			}
		}
	}

	if len(errPodKeys) != 0 {
		return fmt.Errorf("some pod throttle restore failed,err: %s", strings.Join(errPodKeys, ";"))
	}

	return nil
}

func (t *ThrottleExecutor) restorePods(ctx *ExecuteContext, totalReleasedResource *ReleaseResource) (errPodKeys []string) {
	for i := range t.ThrottleUpPods {
		errKeys, _ := t.restoreOnePod(ctx, i, totalReleasedResource)
		errPodKeys = append(errPodKeys, errKeys...)
	}
	return
}

func (t *ThrottleExecutor) restoreOnePod(ctx *ExecuteContext, index int, totalReleasedResource *ReleaseResource) (errPodKeys []string, released ReleaseResource) {
	pod, err := ctx.PodLister.Pods(t.ThrottleUpPods[index].PodKey.Namespace).Get(t.ThrottleUpPods[index].PodKey.Name)
	if err != nil {
		errPodKeys = append(errPodKeys, "not found ", t.ThrottleUpPods[index].PodKey.String())
		return
	}

	// Restore for CPU metric
	for _, v := range t.ThrottleUpPods[index].ContainerCPUUsages {

		// pause container to skip
		if v.ContainerName == "" {
			continue
		}

		klog.V(6).Infof("ThrottleExecutor restore container %s/%s", klog.KObj(pod), v.ContainerName)

		containerCPUQuota, err := podinfo.GetUsageById(t.ThrottleUpPods[index].ContainerCPUQuotas, v.ContainerId)
		if err != nil {
			errPodKeys = append(errPodKeys, err.Error(), t.ThrottleUpPods[index].PodKey.String())
			continue
		}

		containerCPUPeriod, err := podinfo.GetUsageById(t.ThrottleUpPods[index].ContainerCPUPeriods, v.ContainerId)
		if err != nil {
			errPodKeys = append(errPodKeys, err.Error(), t.ThrottleUpPods[index].PodKey.String())
			continue
		}

		container, err := utils.GetPodContainerByName(pod, v.ContainerName)
		if err != nil {
			errPodKeys = append(errPodKeys, err.Error(), t.ThrottleUpPods[index].PodKey.String())
			continue
		}

		var containerCPUQuotaNew float64
		if utils.AlmostEqual(containerCPUQuota.Value, -1.0) || utils.AlmostEqual(containerCPUQuota.Value, 0.0) {
			continue
		} else {
			containerCPUQuotaNew = containerCPUQuota.Value / containerCPUPeriod.Value * (1.0 + float64(t.ThrottleUpPods[index].CPUThrottle.StepCPURatio)/MaxRatio)
		}

		if limitCPU, ok := container.Resources.Limits[v1.ResourceCPU]; ok {
			if float64(limitCPU.MilliValue())/CpuQuotaCoefficient < containerCPUQuotaNew {
				containerCPUQuotaNew = float64(limitCPU.MilliValue()) / CpuQuotaCoefficient
			}
		} else {
			usage, hasExtRes := utils.GetExtCpuRes(container)
			if hasExtRes {
				containerCPUQuotaNew = float64(usage.MilliValue()) / CpuQuotaCoefficient
			}
			if !hasExtRes && containerCPUQuotaNew > MaxUpQuota*containerCPUPeriod.Value/CpuQuotaCoefficient {
				containerCPUQuotaNew = -1
			}

		}

		klog.V(6).Infof("Prior update container resources containerCPUQuotaNew %.2f,containerCPUQuota %.2f,containerCPUPeriod %.2f",
			klog.KObj(pod), containerCPUQuotaNew, containerCPUQuota.Value, containerCPUPeriod.Value)

		if !utils.AlmostEqual(containerCPUQuotaNew*containerCPUPeriod.Value, containerCPUQuota.Value) {

			if utils.AlmostEqual(containerCPUQuotaNew, -1) {
				err = cruntime.UpdateContainerResources(ctx.RuntimeClient, v.ContainerId, cruntime.UpdateOptions{CPUQuota: int64(-1)})
				if err != nil {
					errPodKeys = append(errPodKeys, fmt.Sprintf("Failed to updateResource, err %s", err.Error()), t.ThrottleUpPods[index].PodKey.String())
					continue
				}
			} else {
				err = cruntime.UpdateContainerResources(ctx.RuntimeClient, v.ContainerId, cruntime.UpdateOptions{CPUQuota: int64(containerCPUQuotaNew * containerCPUPeriod.Value)})
				if err != nil {
					klog.Errorf("Failed to updateResource, err %s", err.Error())
					errPodKeys = append(errPodKeys, fmt.Sprintf("Failed to updateResource, err %s", err.Error()), t.ThrottleUpPods[index].PodKey.String())
					continue
				}
			}
		}
		released = ConstructRelease(t.ThrottleUpPods[index], containerCPUQuotaNew, v.Value)
		totalReleasedResource.Add(released)

		t.ThrottleUpPods[index].HasBeenActioned = true
	}

	// Restore other resource, such as memory

	return
}
