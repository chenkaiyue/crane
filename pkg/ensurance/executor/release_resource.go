package executor

import podinfo "github.com/gocrane/crane/pkg/ensurance/executor/pod-info"

type ReleaseResource struct {
	CpuUsage float64
	MemUsage float64
}

// TODO: add memory usage
func ConstructRelease(pod podinfo.PodContext, containerCPUQuotaNew, currentContainerCpuUsage float64) ReleaseResource {
	if pod.PodType == podinfo.Evict {
		return ReleaseResource{
			CpuUsage: pod.PodCPUUsage,
		}
	}
	if pod.PodType == podinfo.ThrottleDown {
		reduction := currentContainerCpuUsage - containerCPUQuotaNew
		if reduction > 0 {
			return ReleaseResource{
				CpuUsage: reduction,
			}
		}
		return ReleaseResource{}
	}
	if pod.PodType == podinfo.ThrottleUp {
		reduction := containerCPUQuotaNew - currentContainerCpuUsage
		if reduction > 0 {
			return ReleaseResource{
				CpuUsage: reduction,
			}
		}
		return ReleaseResource{}
	}
	return ReleaseResource{}
}

func (r *ReleaseResource) Add(new ReleaseResource) {
	r.MemUsage += new.MemUsage
	r.CpuUsage += new.CpuUsage
}
