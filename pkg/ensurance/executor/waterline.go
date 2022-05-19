package executor

import (
	"math"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	"github.com/gocrane/crane/pkg/common"
	"github.com/gocrane/crane/pkg/ensurance/collector/types"
)

// Metrics that can be measured for waterLine
type WaterLineMetric string

// Be consistent with metrics in collector/types/types.go
const (
	CpuUsage = WaterLineMetric(types.MetricNameCpuTotalUsage)
	MemUsage = WaterLineMetric(types.MetricNameMemoryTotalUsage)
)

const (
	// We can't get current use, so can't do actions precisely, just evict every evictedPod
	MissedCurrentUsage float64 = math.MaxFloat64
)

var (
	EvictMetricsCanBeQualified = []WaterLineMetric{CpuUsage, MemUsage}
	ThrottleMetricsCanBeQualified = []WaterLineMetric{CpuUsage}
)

// An WaterLine is a min-heap of Quantity. The values come from each objectiveEnsurance.metricRule.value
type WaterLine []resource.Quantity

func (w WaterLine) Len() int {
	return len(w)
}

func (w WaterLine) Swap(i, j int) {
	w[i], w[j] = w[j], w[i]
}

func (w *WaterLine) Push(x interface{}) {
	*w = append(*w, x.(resource.Quantity))
}

func (w *WaterLine) Pop() interface{} {
	old := *w
	n := len(old)
	x := old[n-1]
	*w = old[0 : n-1]
	return x
}

func (w *WaterLine) PopSmallest() *resource.Quantity {
	wl := *w
	return &wl[0]
}

func (w WaterLine) Less(i, j int) bool {
	cmp := w[i].Cmp(w[j])
	if cmp == -1 {
		return true
	}
	return false
}

func (w WaterLine) String() string {
	str := ""
	for i := 0; i < w.Len(); i++ {
		str += w[i].String()
		str += " "
	}
	return str
}

// GapToWaterLines's key is metric name, value is the difference between usage and the smallest waterline
type GapToWaterLines map[string]float64

// Only calculate gap for metrics that can be quantified
func buildGapToWaterLine(stateMap map[string][]common.TimeSeries,
	throttleExecutor ThrottleExecutor, evictExecutor EvictExecutor) (
	throttleDownGapToWaterLines, throttleUpGapToWaterLines, eviceGapToWaterLines GapToWaterLines) {

	throttleDownGapToWaterLines, throttleUpGapToWaterLines, eviceGapToWaterLines = make(map[string]float64), make(map[string]float64), make(map[string]float64)

	for _,m := range EvictMetricsCanBeQualified {
		// Get the series for each metric
		series, ok := stateMap[string(m)]
		if !ok {
			klog.Warningf("Metric %s not found from collector stateMap", string(m))
			// Can't get current usage, so can not do actions precisely, just evict every evictedPod;
			eviceGapToWaterLines[string(m)] = MissedCurrentUsage
			continue
		}

		// Find the biggest used value
		var maxUsed float64
		if series[0].Samples[0].Value > maxUsed {
			maxUsed = series[0].Samples[0].Value
		}

		// Get the waterLine for each metric in WaterLineMetricsCanBeQualified
		evictWaterLine, evictExist := evictExecutor.EvictWaterLine[string(m)]

		// If metric not exist in EvictWaterLine, eviceGapToWaterLines of metric will can't be calculated
		if !evictExist {
			delete(eviceGapToWaterLines, string(m))
		} else {
			eviceGapToWaterLines[string(m)] = maxUsed - float64(evictWaterLine.PopSmallest().Value())
		}
	}

	for _, m := range ThrottleMetricsCanBeQualified {
		// Get the series for each metric
		series, ok := stateMap[string(m)]
		if !ok {
			klog.Warningf("Metric %s not found from collector stateMap", string(m))
			// Can't get current usage, so can not do actions precisely, just evict every evictedPod;
			throttleDownGapToWaterLines[string(m)] = MissedCurrentUsage
			throttleUpGapToWaterLines[string(m)] = MissedCurrentUsage
			continue
		}

		// Find the biggest used value
		var maxUsed float64
		if series[0].Samples[0].Value > maxUsed {
			maxUsed = series[0].Samples[0].Value
		}

		// Get the waterLine for each metric in WaterLineMetricsCanBeQualified
		throttleDownWaterLine, throttleDownExist := throttleExecutor.ThrottleDownWaterLine[string(m)]
		throttleUpWaterLine, throttleUpExist := throttleExecutor.ThrottleUpWaterLine[string(m)]

		// If a metric does not exist in ThrottleDownWaterLine, throttleDownGapToWaterLines of this metric will can't be calculated
		if !throttleDownExist {
			delete(throttleDownGapToWaterLines, string(m))
		} else {
			throttleDownGapToWaterLines[string(m)] = maxUsed - float64(throttleDownWaterLine.PopSmallest().Value())
		}

		// If metric not exist in ThrottleUpWaterLine, throttleUpGapToWaterLines of metric will can't be calculated
		if !throttleUpExist {
			delete(throttleUpGapToWaterLines, string(m))
		} else {
			// Attention: different with throttleDown and evict
			throttleUpGapToWaterLines[string(m)] = float64(throttleUpWaterLine.PopSmallest().Value()) - maxUsed
		}
	}
	return
}

// Whether no gaps in GapToWaterLines
func (g GapToWaterLines) GapsAllRemoved() bool {
	for _, v := range g {
		if v > 0 {
			return false
		}
	}
	return true
}

// For a specified metric in GapToWaterLines, whether there still has gap
func (g GapToWaterLines) TargetGapsRemoved(metric WaterLineMetric) bool {
	val, ok := g[string(metric)]
	if !ok {
		return true
	}
	if val <= 0 {
		return true
	}
	return false
}

// Whether there is a metric that can't get usage in GapToWaterLines
func (g GapToWaterLines) HasUsageMissedMetric() bool {
	for _, v := range g {
		if v == MissedCurrentUsage {
			return true
		}
	}
	return false
}

// WaterLines 's key is the metric name, value is waterline which get from each objectiveEnsurance.metricRule.value
type WaterLines map[string]*WaterLine

// HasMetricNotInCanbeQualified judges that if there are metrics in WaterLines e that not exist in EvictMetricsCanBeQualified/ThrottleMetricsCanBeQualified
func (e WaterLines) HasMetricNotInCanbeQualified(MetricsCanBeQualified []WaterLineMetric) bool {
	for metric := range e {
		var existInWaterLineMetrics = false
		for _, v := range MetricsCanBeQualified {
			if metric == string(v) {
				existInWaterLineMetrics = true
				break
			}
		}
		if !existInWaterLineMetrics {
			return true
		}
	}
	return false
}
