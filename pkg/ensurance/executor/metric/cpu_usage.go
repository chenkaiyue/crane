package metric

import (
	"fmt"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/gocrane/crane/pkg/ensurance/executor"
	podinfo "github.com/gocrane/crane/pkg/ensurance/executor/pod-info"
	"github.com/gocrane/crane/pkg/ensurance/executor/sort"
	cruntime "github.com/gocrane/crane/pkg/ensurance/runtime"
	"github.com/gocrane/crane/pkg/metrics"
	"github.com/gocrane/crane/pkg/utils"
)

func init() {
	registerMetricMap(cpu)
}

var cpu = metric{
	Name:     executor.CpuUsage,
	SortAble: true,
	SortFunc: sort.CpuMetricsSorter,

	ThrottleAble:      true,
	ThrottleQualified: true,
	ThrottleFunc:      throttleOnePodCpu,
	RestoreFunc:       restoreOnePodCpu,

	EvictAble:      true,
	EvictQualified: true,
	EvictFunc:      evictOnePodCpu,
}

func throttleOnePodCpu(ctx *executor.ExecuteContext, index int, ThrottleDownPods executor.ThrottlePods, totalReleasedResource *executor.ReleaseResource) (errPodKeys []string, released executor.ReleaseResource) {
	pod, err := ctx.PodLister.Pods(ThrottleDownPods[index].PodKey.Namespace).Get(ThrottleDownPods[index].PodKey.Name)
	if err != nil {
		errPodKeys = append(errPodKeys, fmt.Sprintf("pod %s not found", ThrottleDownPods[index].PodKey.String()))
		return
	}

	// Throttle for CPU metrics

	for _, v := range ThrottleDownPods[index].ContainerCPUUsages {
		// pause container to skip
		if v.ContainerName == "" {
			continue
		}

		klog.V(4).Infof("ThrottleExecutor1 avoid container %s/%s", klog.KObj(pod), v.ContainerName)

		containerCPUQuota, err := podinfo.GetUsageById(ThrottleDownPods[index].ContainerCPUQuotas, v.ContainerId)
		if err != nil {
			errPodKeys = append(errPodKeys, err.Error(), ThrottleDownPods[index].PodKey.String())
			continue
		}

		containerCPUPeriod, err := podinfo.GetUsageById(ThrottleDownPods[index].ContainerCPUPeriods, v.ContainerId)
		if err != nil {
			errPodKeys = append(errPodKeys, err.Error(), ThrottleDownPods[index].PodKey.String())
			continue
		}

		container, err := utils.GetPodContainerByName(pod, v.ContainerName)
		if err != nil {
			errPodKeys = append(errPodKeys, err.Error(), ThrottleDownPods[index].PodKey.String())
			continue
		}

		var containerCPUQuotaNew float64
		if utils.AlmostEqual(containerCPUQuota.Value, -1.0) || utils.AlmostEqual(containerCPUQuota.Value, 0.0) {
			containerCPUQuotaNew = v.Value * (1.0 - float64(ThrottleDownPods[index].CPUThrottle.StepCPURatio)/executor.MaxRatio)
		} else {
			containerCPUQuotaNew = containerCPUQuota.Value / containerCPUPeriod.Value * (1.0 - float64(ThrottleDownPods[index].CPUThrottle.StepCPURatio)/executor.MaxRatio)
		}

		if requestCPU, ok := container.Resources.Requests[v1.ResourceCPU]; ok {
			if float64(requestCPU.MilliValue())/executor.CpuQuotaCoefficient > containerCPUQuotaNew {
				containerCPUQuotaNew = float64(requestCPU.MilliValue()) / executor.CpuQuotaCoefficient
			}
		}

		if limitCPU, ok := container.Resources.Limits[v1.ResourceCPU]; ok {
			if float64(limitCPU.MilliValue())/executor.CpuQuotaCoefficient*float64(ThrottleDownPods[index].CPUThrottle.MinCPURatio)/executor.MaxRatio > containerCPUQuotaNew {
				containerCPUQuotaNew = float64(limitCPU.MilliValue()) * float64(ThrottleDownPods[index].CPUThrottle.MinCPURatio) / executor.CpuQuotaCoefficient
			}
		}

		klog.V(6).Infof("Prior update container resources containerCPUQuotaNew %.2f, containerCPUQuota.Value %.2f,containerCPUPeriod %.2f",
			containerCPUQuotaNew, containerCPUQuota.Value, containerCPUPeriod.Value)

		if !utils.AlmostEqual(containerCPUQuotaNew*containerCPUPeriod.Value, containerCPUQuota.Value) {
			err = cruntime.UpdateContainerResources(ctx.RuntimeClient, v.ContainerId, cruntime.UpdateOptions{CPUQuota: int64(containerCPUQuotaNew * containerCPUPeriod.Value)})
			if err != nil {
				errPodKeys = append(errPodKeys, fmt.Sprintf("failed to updateResource for %s/%s, error: %v", ThrottleDownPods[index].PodKey.String(), v.ContainerName, err))
				continue
			} else {
				klog.V(4).Infof("ThrottleExecutor avoid pod %s, container %s, set cpu quota %.2f.",
					klog.KObj(pod), v.ContainerName, containerCPUQuotaNew)
			}
		}
		released = ConstructCpuUsageRelease(ThrottleDownPods[index], containerCPUQuotaNew, v.Value)
		totalReleasedResource.Add(released)
	}
	return
}

func restoreOnePodCpu(ctx *executor.ExecuteContext, index int, ThrottleUpPods executor.ThrottlePods, totalReleasedResource *executor.ReleaseResource) (errPodKeys []string, released executor.ReleaseResource) {
	pod, err := ctx.PodLister.Pods(ThrottleUpPods[index].PodKey.Namespace).Get(ThrottleUpPods[index].PodKey.Name)
	if err != nil {
		errPodKeys = append(errPodKeys, "not found ", ThrottleUpPods[index].PodKey.String())
		return
	}

	// Restore for CPU metric
	for _, v := range ThrottleUpPods[index].ContainerCPUUsages {

		// pause container to skip
		if v.ContainerName == "" {
			continue
		}

		klog.V(6).Infof("ThrottleExecutor restore container %s/%s", klog.KObj(pod), v.ContainerName)

		containerCPUQuota, err := podinfo.GetUsageById(ThrottleUpPods[index].ContainerCPUQuotas, v.ContainerId)
		if err != nil {
			errPodKeys = append(errPodKeys, err.Error(), ThrottleUpPods[index].PodKey.String())
			continue
		}

		containerCPUPeriod, err := podinfo.GetUsageById(ThrottleUpPods[index].ContainerCPUPeriods, v.ContainerId)
		if err != nil {
			errPodKeys = append(errPodKeys, err.Error(), ThrottleUpPods[index].PodKey.String())
			continue
		}

		container, err := utils.GetPodContainerByName(pod, v.ContainerName)
		if err != nil {
			errPodKeys = append(errPodKeys, err.Error(), ThrottleUpPods[index].PodKey.String())
			continue
		}

		var containerCPUQuotaNew float64
		if utils.AlmostEqual(containerCPUQuota.Value, -1.0) || utils.AlmostEqual(containerCPUQuota.Value, 0.0) {
			continue
		} else {
			containerCPUQuotaNew = containerCPUQuota.Value / containerCPUPeriod.Value * (1.0 + float64(ThrottleUpPods[index].CPUThrottle.StepCPURatio)/executor.MaxRatio)
		}

		if limitCPU, ok := container.Resources.Limits[v1.ResourceCPU]; ok {
			if float64(limitCPU.MilliValue())/executor.CpuQuotaCoefficient < containerCPUQuotaNew {
				containerCPUQuotaNew = float64(limitCPU.MilliValue()) / executor.CpuQuotaCoefficient
			}
		} else {
			usage, hasExtRes := utils.GetExtCpuRes(container)
			if hasExtRes {
				containerCPUQuotaNew = float64(usage.MilliValue()) / executor.CpuQuotaCoefficient
			}
			if !hasExtRes && containerCPUQuotaNew > executor.MaxUpQuota*containerCPUPeriod.Value/executor.CpuQuotaCoefficient {
				containerCPUQuotaNew = -1
			}

		}

		klog.V(6).Infof("Prior update container resources containerCPUQuotaNew %.2f,containerCPUQuota %.2f,containerCPUPeriod %.2f",
			klog.KObj(pod), containerCPUQuotaNew, containerCPUQuota.Value, containerCPUPeriod.Value)

		if !utils.AlmostEqual(containerCPUQuotaNew*containerCPUPeriod.Value, containerCPUQuota.Value) {

			if utils.AlmostEqual(containerCPUQuotaNew, -1) {
				err = cruntime.UpdateContainerResources(ctx.RuntimeClient, v.ContainerId, cruntime.UpdateOptions{CPUQuota: int64(-1)})
				if err != nil {
					errPodKeys = append(errPodKeys, fmt.Sprintf("Failed to updateResource, err %s", err.Error()), ThrottleUpPods[index].PodKey.String())
					continue
				}
			} else {
				err = cruntime.UpdateContainerResources(ctx.RuntimeClient, v.ContainerId, cruntime.UpdateOptions{CPUQuota: int64(containerCPUQuotaNew * containerCPUPeriod.Value)})
				if err != nil {
					klog.Errorf("Failed to updateResource, err %s", err.Error())
					errPodKeys = append(errPodKeys, fmt.Sprintf("Failed to updateResource, err %s", err.Error()), ThrottleUpPods[index].PodKey.String())
					continue
				}
			}
		}
		released = ConstructCpuUsageRelease(ThrottleUpPods[index], containerCPUQuotaNew, v.Value)
		totalReleasedResource.Add(released)

		ThrottleUpPods[index].HasBeenActioned = true
	}

	return
}

func evictOnePodCpu(wg *sync.WaitGroup, ctx *executor.ExecuteContext, index int, totalReleasedResource *executor.ReleaseResource, EvictPods executor.EvictPods) (errPodKeys []string, released executor.ReleaseResource) {
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
		released = ConstructCpuUsageRelease(evictPod, 0.0, 0.0)
		totalReleasedResource.Add(released)
	}(EvictPods[index])
	return
}

func ConstructCpuUsageRelease(pod podinfo.PodContext, containerCPUQuotaNew, currentContainerCpuUsage float64) executor.ReleaseResource {
	if pod.PodType == podinfo.Evict {
		return executor.ReleaseResource{
			executor.CpuUsage: pod.PodCPUUsage,
		}
	}
	if pod.PodType == podinfo.ThrottleDown {
		reduction := currentContainerCpuUsage - containerCPUQuotaNew
		if reduction > 0 {
			return executor.ReleaseResource{
				executor.CpuUsage: reduction,
			}
		}
		return executor.ReleaseResource{}
	}
	if pod.PodType == podinfo.ThrottleUp {
		reduction := containerCPUQuotaNew - currentContainerCpuUsage
		if reduction > 0 {
			return executor.ReleaseResource{
				executor.CpuUsage: reduction,
			}
		}
		return executor.ReleaseResource{}
	}
	return executor.ReleaseResource{}
}
