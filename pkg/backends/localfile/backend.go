package localfile

import (
	"fmt"
	"math/rand"
	"os"
	"path"
	"sort"
	"sync"

	jsoniter "github.com/json-iterator/go"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	"github.com/irvinlim/apple-health-ingester/pkg/backends"
	"github.com/irvinlim/apple-health-ingester/pkg/healthautoexport"
)

var (
	metricsPath  string
	workoutsPath string
)

// Backend LocalFile is used to store ingested metrics and workouts in the local
// filesystem as JSON files. It is not very performant as it would process all
// data at once to produce a sorted JSON output file. As such, it should only be
// used for debugging purposes.
//
// Metrics are written as one file per metric, merged by datapoint timestamp.
// Workouts are written to a single file, merged by workout start time, and are
// only handled when --localfile.workoutsPath is set.
type Backend struct {
	metrics  map[string]*MetricFile
	workouts *WorkoutFile
	mtx      sync.RWMutex
}

var _ backends.Backend = &Backend{}

func NewBackend() (*Backend, error) {
	backend := &Backend{}

	// Load metrics
	if metricsPath == "" {
		return nil, errors.New("--localfile.metricsPath is not set")
	}
	metrics, err := backend.loadMetrics()
	if err != nil {
		return nil, errors.Wrapf(err, "cannot load metrics from %v", metricsPath)
	}
	backend.metrics = metrics

	// Load workouts, if this backend is configured to handle them.
	if workoutsPath != "" {
		workouts, err := backend.loadWorkouts()
		if err != nil {
			return nil, errors.Wrapf(err, "cannot load workouts from %v", workoutsPath)
		}
		backend.workouts = workouts
	}

	return backend, nil
}

func (b *Backend) Name() string {
	return "LocalFile"
}

// Write will take the incoming payload and merge the metrics and workouts with
// existing data, before writing it back to the filesystem.
func (b *Backend) Write(payload *healthautoexport.Payload, target string) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()

	// Handle metrics.
	for _, metric := range payload.Data.Metrics {
		if err := b.handleMetric(metric, target); err != nil {
			return errors.Wrapf(err, "handle metric error for %v", metric.Name)
		}
	}

	// Handle workouts.
	if len(payload.Data.Workouts) > 0 {
		if workoutsPath == "" {
			// Surface the misconfiguration rather than silently discarding data.
			log.Warnf("discarding %d workout(s): --localfile.workoutsPath is not set",
				len(payload.Data.Workouts))
		} else if err := b.handleWorkouts(payload.Data.Workouts, target); err != nil {
			return errors.Wrapf(err, "handle workouts error for %d workout(s)",
				len(payload.Data.Workouts))
		}
	}

	return nil
}

func (b *Backend) handleMetric(metric *healthautoexport.Metric, target string) error {
	var metricFile MetricFile
	metricFile.FromMetric(metric, target)
	fileName := metricFile.GetFileName()
	updatedData := metricFile.Data

	// Merge with existing data if present
	existing, ok := b.metrics[fileName]
	if ok {
		// Merge data points by timestamp
		dataByTimestamp := make(map[healthautoexport.Time]*healthautoexport.Datapoint, len(existing.Data))
		for _, datum := range existing.Data {
			dataByTimestamp[*datum.Date] = datum
		}
		for _, datum := range metricFile.Data {
			dataByTimestamp[*datum.Date] = datum
		}

		// Convert back to slice and sort
		newData := make([]*healthautoexport.Datapoint, 0, len(dataByTimestamp))
		for _, datapoint := range dataByTimestamp {
			newData = append(newData, datapoint)
		}
		sort.Slice(newData, func(i, j int) bool {
			return newData[i].Date.Before(newData[j].Date.Time)
		})

		// Store merged data
		updatedData = newData
	}

	// Update data
	metricFile.Data = updatedData

	// Write back
	metricFilePath := path.Join(metricsPath, fileName)
	if err := b.writeMetricFile(metricFilePath, &metricFile); err != nil {
		return errors.Wrapf(err, "cannot write metrics to %v", metricFilePath)
	}

	// Keep the in-memory state in sync so later requests merge with every
	// datapoint accepted since startup, not only with the startup snapshot.
	b.metrics[fileName] = &metricFile

	return nil
}

// handleWorkouts merges the incoming workouts with the workouts already loaded
// from disk, deduplicating by start time, then writes the merged set back out.
//
// Health Auto Export re-exports overlapping windows (for example an automation
// reporting "Today"), so the same workout is normally received many times. Start
// time is used as the identity because the exported workout payload carries no
// stable id, and a re-export of the same workout is identical in name and start.
func (b *Backend) handleWorkouts(workouts []*healthautoexport.Workout, target string) error {
	if b.workouts == nil {
		b.workouts = &WorkoutFile{}
	}

	byStart := make(map[string]*healthautoexport.Workout, len(b.workouts.Data))
	for _, workout := range b.workouts.Data {
		if key, ok := workoutKey(workout); ok {
			byStart[key] = workout
		}
	}

	skipped := 0
	for _, workout := range workouts {
		key, ok := workoutKey(workout)
		if !ok {
			skipped++
			continue
		}
		byStart[key] = workout
	}
	if skipped > 0 {
		log.Warnf("skipped %d workout(s) without a start time", skipped)
	}

	merged := make([]*healthautoexport.Workout, 0, len(byStart))
	for _, workout := range byStart {
		merged = append(merged, workout)
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Start.Before(merged[j].Start.Time)
	})

	b.workouts.Data = merged

	workoutsFilePath := path.Join(workoutsPath, b.workouts.GetFileName())
	if err := writeJSONFile(workoutsFilePath, b.workouts); err != nil {
		return errors.Wrapf(err, "cannot write workouts to %v", workoutsFilePath)
	}

	return nil
}

// workoutKey returns a stable identity for a workout, which is its start time in
// RFC3339. A workout without a start time cannot be identified across
// re-exports, and is therefore not stored.
func workoutKey(workout *healthautoexport.Workout) (string, bool) {
	if workout == nil || workout.Start.IsZero() {
		return "", false
	}
	return workout.Start.String(), true
}

func (b *Backend) loadMetrics() (map[string]*MetricFile, error) {
	output := make(map[string]*MetricFile)
	files, err := os.ReadDir(metricsPath)
	if err != nil {
		// Directory doesn't exist, simply return empty map.
		if os.IsNotExist(err) {
			return output, nil
		}

		return nil, errors.Wrapf(err, "cannot read dir")
	}

	for _, file := range files {
		metricFilePath := path.Join(metricsPath, file.Name())
		metricFile, err := b.loadMetricFile(metricFilePath)
		if err != nil {
			log.WithError(err).Warnf("could not read %v as metric file", metricFilePath)
			continue
		}
		output[metricFile.GetFileName()] = metricFile
	}

	return output, nil
}

func (b *Backend) loadMetricFile(name string) (*MetricFile, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot open %v", name)
	}
	defer func() {
		_ = file.Close()
	}()

	var metricFile MetricFile
	dec := jsoniter.NewDecoder(file)
	if err := dec.Decode(&metricFile); err != nil {
		return nil, err
	}

	return &metricFile, nil
}

func (b *Backend) loadWorkouts() (*WorkoutFile, error) {
	workoutsFilePath := path.Join(workoutsPath, WorkoutFile{}.GetFileName())

	file, err := os.Open(workoutsFilePath)
	if err != nil {
		// File doesn't exist yet, simply return an empty file.
		if os.IsNotExist(err) {
			return &WorkoutFile{}, nil
		}
		return nil, errors.Wrapf(err, "cannot open %v", workoutsFilePath)
	}
	defer func() {
		_ = file.Close()
	}()

	var workouts WorkoutFile
	dec := jsoniter.NewDecoder(file)
	if err := dec.Decode(&workouts); err != nil {
		return nil, err
	}

	return &workouts, nil
}

func (b *Backend) writeMetricFile(name string, metricFile *MetricFile) error {
	// Ensure directories exist
	dirname := path.Dir(name)
	if err := os.MkdirAll(dirname, 0755); err != nil {
		return errors.Wrapf(err, "cannot makedirs for %v", dirname)
	}

	// Write file
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return errors.Wrapf(err, "cannot open %v", name)
	}
	defer func() {
		_ = file.Close()
	}()

	// Encode as JSON
	enc := jsoniter.NewEncoder(file)
	enc.SetIndent("", "  ")
	return enc.Encode(metricFile)
}

// writeJSONFile writes value to name as indented JSON, via a temporary file and
// rename, so that a reader never observes a partially written file.
func writeJSONFile(name string, value interface{}) error {
	// Ensure directories exist
	dirname := path.Dir(name)
	if err := os.MkdirAll(dirname, 0755); err != nil {
		return errors.Wrapf(err, "cannot makedirs for %v", dirname)
	}

	tmp, err := openTempFile(dirname, path.Base(name))
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup; a successful rename has already moved the file.
		_ = os.Remove(tmpName)
	}()

	enc := jsoniter.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		_ = tmp.Close()
		return errors.Wrapf(err, "cannot encode %v", name)
	}
	if err := tmp.Close(); err != nil {
		return errors.Wrapf(err, "cannot close %v", tmpName)
	}
	if err := os.Rename(tmpName, name); err != nil {
		return errors.Wrapf(err, "cannot rename %v to %v", tmpName, name)
	}

	return nil
}

// openTempFile creates a new unique temporary file alongside name.
//
// It deliberately does not use os.CreateTemp: that helper creates the file
// with a fixed mode of 0600, which is unaffected by the process umask, and the
// caller previously had to chmod the result to 0644 explicitly. That produced
// files with a wider mode than every other file this backend writes.
//
// Opening with a 0644 create mode instead lets the process umask apply, so
// workouts.json matches the metrics files (0640 under the service's UMask of
// 0027). O_EXCL guarantees a fresh file, so a stale leftover can never be reused
// or truncated in place.
func openTempFile(dirname, base string) (*os.File, error) {
	for attempt := 0; attempt < 100; attempt++ {
		tmpName := path.Join(dirname, fmt.Sprintf(".%s.tmp.%d", base, rand.Int63()))
		file, err := os.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err == nil {
			return file, nil
		}
		if !os.IsExist(err) {
			return nil, errors.Wrapf(err, "cannot create temp file in %v", dirname)
		}
	}
	return nil, errors.Errorf("cannot create temp file in %v: exhausted unique names", dirname)
}

func init() {
	pflag.StringVar(&metricsPath, "localfile.metricsPath", "",
		"Output path to write metrics, with one metric per file. All data will be aggregated by timestamp. "+
			"Any existing data will be merged together.")

	pflag.StringVar(&workoutsPath, "localfile.workoutsPath", "",
		"Output path to write workouts, as a single file merged by workout start time. "+
			"Any existing data will be merged together. If unset, workouts are discarded.")
}
