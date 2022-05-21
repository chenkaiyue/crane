package executor

type ReleaseResource map[WaterLineMetric]float64

func (r ReleaseResource) Add(new ReleaseResource) {
	for metric, value := range new {
		r[metric] += value
	}
}
