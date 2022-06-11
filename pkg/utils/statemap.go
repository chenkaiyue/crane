package utils

import (
	"github.com/gocrane/crane/pkg/common"
	"k8s.io/klog/v2"
)

func StateMapInfo(stateMap map[string][]common.TimeSeries) {
	klog.V(6).Info("stateMap Info:")
	for m, tss := range stateMap {
		klog.V(6).Infof("Metric %s", m)
		for _, ts := range tss {
			klog.V(6).Infof("labels are %s, values are %s", ts.Labels, ts.Samples)
		}
	}
}
