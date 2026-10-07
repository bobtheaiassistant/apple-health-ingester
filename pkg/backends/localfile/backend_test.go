package localfile

import (
	"encoding/json"
	"os"
	"path"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/irvinlim/apple-health-ingester/pkg/healthautoexport"
	"github.com/irvinlim/apple-health-ingester/pkg/healthautoexport/fixtures"
)

// setupBackendPaths points both backend paths at a fresh directory, restoring
// the package-level flags afterwards since they are process-wide.
func setupBackendPaths(t *testing.T, withWorkoutsPath bool) string {
	t.Helper()

	dir := t.TempDir()
	metricsPath = dir
	workoutsPath = ""
	if withWorkoutsPath {
		workoutsPath = dir
	}

	t.Cleanup(func() {
		metricsPath = ""
		workoutsPath = ""
	})

	return dir
}

func readWorkoutFile(t *testing.T, dir string) WorkoutFile {
	t.Helper()

	raw, err := os.ReadFile(path.Join(dir, "workouts.json"))
	require.NoError(t, err)

	var file WorkoutFile
	require.NoError(t, json.Unmarshal(raw, &file))

	return file
}

func readMetricFile(t *testing.T, dir, name string) MetricFile {
	t.Helper()

	raw, err := os.ReadFile(path.Join(dir, name))
	require.NoError(t, err)

	var file MetricFile
	require.NoError(t, json.Unmarshal(raw, &file))

	return file
}

func mkworkout(t *testing.T, name, start, end string) *healthautoexport.Workout {
	t.Helper()

	startTime, err := healthautoexport.ParseTime(start)
	require.NoError(t, err)
	endTime, err := healthautoexport.ParseTime(end)
	require.NoError(t, err)

	return &healthautoexport.Workout{
		Name:  name,
		Start: &startTime,
		End:   &endTime,
	}
}

func payloadWithWorkouts(workouts ...*healthautoexport.Workout) *healthautoexport.Payload {
	return &healthautoexport.Payload{
		Data: &healthautoexport.PayloadData{
			Workouts: workouts,
		},
	}
}

func TestWriteStoresWorkouts(t *testing.T) {
	dir := setupBackendPaths(t, true)

	backend, err := NewBackend()
	require.NoError(t, err)

	require.NoError(t, backend.Write(fixtures.PayloadWithWorkouts, ""))

	file := readWorkoutFile(t, dir)
	require.Len(t, file.Data, 1)
	assert.Equal(t, "Walking", file.Data[0].Name)
	assert.Equal(t, "2021-12-24T08:02:43+08:00", file.Data[0].Start.String())
	assert.NotEmpty(t, file.Data[0].Route, "route data should be preserved")
	assert.NotEmpty(t, file.Data[0].HeartRateData, "heart rate data should be preserved")
	assert.NotEmpty(t, file.Data[0].Fields, "arbitrary workout fields should be preserved")
}

func TestWriteWorkoutsIsIdempotentAcrossReExports(t *testing.T) {
	dir := setupBackendPaths(t, true)

	backend, err := NewBackend()
	require.NoError(t, err)

	// Health Auto Export re-exports overlapping windows, so the same workout
	// arrives repeatedly and must not accumulate duplicates.
	for i := 0; i < 3; i++ {
		require.NoError(t, backend.Write(fixtures.PayloadWithWorkouts, ""))
	}

	assert.Len(t, readWorkoutFile(t, dir).Data, 1)
}

func TestWriteWorkoutsMergesWithExistingFile(t *testing.T) {
	dir := setupBackendPaths(t, true)

	backend, err := NewBackend()
	require.NoError(t, err)

	// A later workout is written first, so the merge also has to sort by start.
	later := mkworkout(t, "Ride", "2021-12-25 09:00:00 +0800", "2021-12-25 10:00:00 +0800")
	require.NoError(t, backend.Write(payloadWithWorkouts(later), ""))

	// Simulate a restart: the backend must pick up workouts already on disk.
	reloaded, err := NewBackend()
	require.NoError(t, err)
	require.NoError(t, reloaded.Write(payloadWithWorkouts(
		mkworkout(t, "Walking", "2021-12-24 08:02:43 +0800", "2021-12-24 08:21:53 +0800"),
	), ""))

	file := readWorkoutFile(t, dir)
	require.Len(t, file.Data, 2)
	assert.Equal(t, []string{"Walking", "Ride"}, []string{file.Data[0].Name, file.Data[1].Name})
}

func TestWriteWorkoutsIsDiscardedWithoutPath(t *testing.T) {
	dir := setupBackendPaths(t, false)

	backend, err := NewBackend()
	require.NoError(t, err)

	// Workouts must not fail the request, but must not be silently written either.
	require.NoError(t, backend.Write(fixtures.PayloadWithWorkouts, ""))

	_, err = os.Stat(path.Join(dir, "workouts.json"))
	assert.True(t, os.IsNotExist(err), "no workout file should be created")
}

func TestWriteWorkoutsSkipsWorkoutsWithoutStart(t *testing.T) {
	dir := setupBackendPaths(t, true)

	backend, err := NewBackend()
	require.NoError(t, err)

	require.NoError(t, backend.Write(payloadWithWorkouts(
		mkworkout(t, "Walking", "2021-12-24 08:02:43 +0800", "2021-12-24 08:21:53 +0800"),
		&healthautoexport.Workout{Name: "NoStart"},
	), ""))

	file := readWorkoutFile(t, dir)
	require.Len(t, file.Data, 1)
	assert.Equal(t, "Walking", file.Data[0].Name)
}

func TestWriteMetricsStillWorkAlongsideWorkouts(t *testing.T) {
	dir := setupBackendPaths(t, true)

	backend, err := NewBackend()
	require.NoError(t, err)

	require.NoError(t, backend.Write(fixtures.PayloadWithWorkouts, ""))
	require.NoError(t, backend.Write(fixtures.PayloadWithMetrics, ""))

	assert.Len(t, readWorkoutFile(t, dir).Data, 1)

	// Metrics must keep their existing one-file-per-metric behaviour.
	metricRaw, err := os.ReadFile(path.Join(dir, "active_energy_kJ.json"))
	require.NoError(t, err)
	assert.Contains(t, string(metricRaw), "active_energy")
}

func TestWriteMetricsMergesAcrossRequestsWithoutRestart(t *testing.T) {
	dir := setupBackendPaths(t, false)

	backend, err := NewBackend()
	require.NoError(t, err)

	// Health Auto Export can send a non-empty daily request followed by an
	// empty request for the same metric. The latter must not erase data that
	// the running backend accepted earlier.
	require.NoError(t, backend.Write(fixtures.PayloadWithMetrics, ""))
	require.NoError(t, backend.Write(&healthautoexport.Payload{
		Data: &healthautoexport.PayloadData{
			Metrics: []*healthautoexport.Metric{
				fixtures.MetricBasalBodyTemperatureNoData,
				{
					Name:  "active_energy",
					Units: "kJ",
				},
			},
		},
	}, ""))

	assert.Len(t, readMetricFile(t, dir, "active_energy_kJ.json").Data, 2)
}

// TestWriteWorkoutsRespectsUmask guards the file mode of workouts.json. The
// service runs with UMask=0027, so every file it writes must land as 0640. The
// workouts writer used to create its temp file with os.CreateTemp (mode 0600)
// and then chmod it to 0644, which bypassed the umask and produced a wider mode
// than the metrics files.
func TestWriteWorkoutsRespectsUmask(t *testing.T) {
	// syscall.Umask is process-wide, so restore it before returning.
	previous := syscall.Umask(0027)
	defer syscall.Umask(previous)

	dir := setupBackendPaths(t, true)

	backend, err := NewBackend()
	require.NoError(t, err)

	require.NoError(t, backend.Write(fixtures.PayloadWithWorkouts, ""))

	info, err := os.Stat(path.Join(dir, "workouts.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0640), info.Mode().Perm(),
		"workouts.json must honour UMask=0027 and be 0640")

	// The file must still be valid JSON containing the ingested workout.
	file := readWorkoutFile(t, dir)
	require.Len(t, file.Data, 1)
	assert.Equal(t, "Walking", file.Data[0].Name)
}
