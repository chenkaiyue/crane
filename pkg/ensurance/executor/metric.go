package executor

import (
	"sync"

	podinfo "github.com/gocrane/crane/pkg/ensurance/executor/pod-info"
)

type metric struct {
	Name WaterLineMetric

	SortAble bool
	SortFunc func(pods []podinfo.PodContext)

	ThrottleAble      bool
	ThrottleQualified bool
	ThrottleFunc      func(ctx *ExecuteContext, index int, ThrottleDownPods ThrottlePods, totalReleasedResource *ReleaseResource) (errPodKeys []string, released ReleaseResource)
	RestoreFunc       func(ctx *ExecuteContext, index int, ThrottleUpPods ThrottlePods, totalReleasedResource *ReleaseResource) (errPodKeys []string, released ReleaseResource)

	EvictAble      bool
	EvictQualified bool
	EvictFunc      func(wg *sync.WaitGroup, ctx *ExecuteContext, index int, totalReleasedResource *ReleaseResource, EvictPods EvictPods) (errPodKeys []string, released ReleaseResource)
}

var MetricMap = make(map[WaterLineMetric]metric)

func registerMetricMap(m metric) {
	MetricMap[m.Name] = m
}

func GetThrottleAbleMetricName() (throttleAbleMetricList []WaterLineMetric) {
	for _, m := range MetricMap {
		if m.ThrottleAble {
			throttleAbleMetricList = append(throttleAbleMetricList, m.Name)
		}
	}
	return
}

func GetEvictAbleMetricName() (evictAbleMetricList []WaterLineMetric) {
	for _, m := range MetricMap {
		if m.EvictAble {
			evictAbleMetricList = append(evictAbleMetricList, m.Name)
		}
	}
	return
}
