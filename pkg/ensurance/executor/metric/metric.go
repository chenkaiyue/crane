package metric

import (
	"sync"

	podinfo "github.com/gocrane/crane/pkg/ensurance/executor/pod-info"

	"github.com/gocrane/crane/pkg/ensurance/executor"
)

type metric struct {
	Name    executor.WaterLineMetric

	SortAble bool
	SortFunc func(pods []podinfo.PodContext)

	ThrottleAble bool
	ThrottleQualified bool
	ThrottleFunc func(ctx *executor.ExecuteContext, index int, totalReleasedResource *executor.ReleaseResource, ThrottleDownPods executor.ThrottlePods) (errPodKeys []string, released executor.ReleaseResource)
	RestoreFunc func(ctx *executor.ExecuteContext, index int, totalReleasedResource *executor.ReleaseResource, ThrottleUpPods executor.ThrottlePods) (errPodKeys []string, released executor.ReleaseResource)

	EvictAble bool
	EvictQualified bool
	EvictFunc func(wg *sync.WaitGroup, ctx *executor.ExecuteContext, index int, totalReleasedResource *executor.ReleaseResource, EvictPods executor.EvictPods) (errPodKeys []string, released executor.ReleaseResource)
}

var MetricMap = make(map[executor.WaterLineMetric]metric)

func registerMetricMap(m metric) {
	MetricMap[m.Name] = m
}

func GetThrottleAbleMetricName() (throttleAbleMetricList []executor.WaterLineMetric) {
	for _, m := range MetricMap {
		if m.ThrottleAble {
			throttleAbleMetricList = append(throttleAbleMetricList, m.Name)
		}
	}
	return
}

func GetEvictAbleMetricName() (evictAbleMetricList []executor.WaterLineMetric) {
	for _, m := range MetricMap {
		if m.EvictAble {
			evictAbleMetricList = append(evictAbleMetricList, m.Name)
		}
	}
	return
}


