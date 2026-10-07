package localfile

import (
	"strings"

	"github.com/irvinlim/apple-health-ingester/pkg/healthautoexport"
)

type MetricFile struct {
	Name   string                        `json:"name"`
	Target string                        `json:"target,omitempty"`
	Units  healthautoexport.Units        `json:"units"`
	Data   []*healthautoexport.Datapoint `json:"data"`
}

// AggregatedSleepMetricFile preserves the richer nightly sleep summaries that
// cannot be represented as ordinary quantity datapoints.
type AggregatedSleepMetricFile struct {
	Name   string                                      `json:"name"`
	Target string                                      `json:"target,omitempty"`
	Units  healthautoexport.Units                      `json:"units"`
	Data   []*healthautoexport.AggregatedSleepAnalysis `json:"data"`
}

// SleepAnalysisMetricFile stores non-aggregated sleep-stage intervals.
type SleepAnalysisMetricFile struct {
	Name   string                            `json:"name"`
	Target string                            `json:"target,omitempty"`
	Units  healthautoexport.Units            `json:"units"`
	Data   []*healthautoexport.SleepAnalysis `json:"data"`
}

func (f MetricFile) GetFileName() string {
	filename := f.Name + "_" + string(f.Units)
	filename = strings.ReplaceAll(filename, "/", "_")
	if f.Target != "" {
		filename = f.Target + "_" + filename
	}
	return filename + ".json"
}

func (f *MetricFile) FromMetric(metric *healthautoexport.Metric, target string) {
	f.Name = metric.Name
	f.Units = metric.Units
	f.Data = metric.Datapoints
	f.Target = target
}

// WorkoutFile is the on-disk representation of ingested workouts. Unlike
// metrics, which are stored as one file per metric name, every workout is kept
// in this single file.
type WorkoutFile struct {
	Data []*healthautoexport.Workout `json:"data"`
}

func (f WorkoutFile) GetFileName() string {
	return "workouts.json"
}
