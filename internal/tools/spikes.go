package tools

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"incident-mcp/internal/backend"
)

// maxSpikeReport caps how many spikes are reported so one noisy series cannot
// dominate a response.
const maxSpikeReport = 20

// maxSeriesPoints caps how many raw samples are echoed back as evidence.
const maxSeriesPoints = 500

// detectorDoc documents the rule applied to a series. The detector is
// deliberately robust: a classic mean + k*stddev threshold is itself inflated
// by the spikes it is supposed to find, so a dense burst of spikes can hide
// from it. Median/MAD is unaffected by how many outliers there are.
const detectorDoc = "value > median + 3.5*(1.4826*MAD); when MAD == 0, value > 1.05*median (any positive value when median <= 0)"

// SpikeStatistics summarises a series and the detector applied to it.
type SpikeStatistics struct {
	Points       int     `json:"points"`
	Mean         float64 `json:"mean"`
	StdDev       float64 `json:"stddev"`
	Median       float64 `json:"median"`
	MAD          float64 `json:"median_absolute_deviation"`
	Min          float64 `json:"min"`
	Max          float64 `json:"max"`
	BaselineMean float64 `json:"baseline_mean"` // mean of the first half of the window
	Threshold    float64 `json:"threshold"`
	Detector     string  `json:"detector"`
	SpikeCount   int     `json:"spike_count"`
}

// Spike is one sample that exceeded the detection threshold.
type Spike struct {
	Timestamp           string  `json:"timestamp"`
	Value               float64 `json:"value"`
	ExcessOverThreshold float64 `json:"excess_over_threshold"`
}

// analyze applies the detector documented in detectorDoc to a window of
// samples and returns statistics plus the spikes it found (chronological,
// capped at maxSpikeReport largest excursions).
func analyze(points []backend.Point) (SpikeStatistics, []Spike) {
	stat := SpikeStatistics{Detector: detectorDoc}
	spikes := []Spike{}
	if len(points) == 0 {
		return stat, spikes
	}

	values := make([]float64, len(points))
	for i, p := range points {
		values[i] = p.V
	}

	var sum, min, max float64
	min, max = values[0], values[0]
	for _, v := range values {
		sum += v
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	mean := sum / float64(len(values))

	var sq float64
	for _, v := range values {
		d := v - mean
		sq += d * d
	}
	stddev := math.Sqrt(sq / float64(len(values)))

	median, mad := medianAndMAD(values)
	threshold := robustThreshold(median, mad)

	half := len(points) / 2
	var baselineSum float64
	for _, p := range points[:half] {
		baselineSum += p.V
	}
	baseline := 0.0
	if half > 0 {
		baseline = baselineSum / float64(half)
	}

	stat = SpikeStatistics{
		Points:       len(points),
		Mean:         roundSig(mean),
		StdDev:       roundSig(stddev),
		Median:       roundSig(median),
		MAD:          roundSig(mad),
		Min:          roundSig(min),
		Max:          roundSig(max),
		BaselineMean: roundSig(baseline),
		Threshold:    roundSig(threshold),
		Detector:     detectorDoc,
	}

	for _, p := range points {
		if p.V > threshold {
			spikes = append(spikes, Spike{
				Timestamp:           p.T.UTC().Format(time.RFC3339),
				Value:               roundSig(p.V),
				ExcessOverThreshold: roundSig(p.V - threshold),
			})
		}
	}
	stat.SpikeCount = len(spikes)

	if len(spikes) > maxSpikeReport {
		// Keep the largest excursions, then restore chronological order.
		sort.SliceStable(spikes, func(i, j int) bool {
			return spikes[i].ExcessOverThreshold > spikes[j].ExcessOverThreshold
		})
		spikes = spikes[:maxSpikeReport]
		sort.SliceStable(spikes, func(i, j int) bool { return spikes[i].Timestamp < spikes[j].Timestamp })
	}
	return stat, spikes
}

// robustThreshold converts a median and MAD into an outlier threshold.
func robustThreshold(median, mad float64) float64 {
	if mad > 0 {
		// Iglewicz & Hoaglin: modified z-score > 3.5 == deviation > 3.5*1.4826*MAD.
		return median + 3.5*1.4826*mad
	}
	if median > 0 {
		// Flat series: allow 5% relative movement before calling it a spike.
		return median * 1.05
	}
	// Flat zero baseline (an idle service): any positive sample is notable.
	return 0
}

func medianAndMAD(values []float64) (median, mad float64) {
	if len(values) == 0 {
		return 0, 0
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	median = percentile(sorted, 0.5)

	devs := make([]float64, len(values))
	for i, v := range values {
		devs[i] = math.Abs(v - median)
	}
	sort.Float64s(devs)
	mad = percentile(devs, 0.5)
	return median, mad
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (pos-float64(lo))*(sorted[hi]-sorted[lo])
}

// summarize condenses a series into scalar evidence.
func summarize(points []backend.Point) map[string]any {
	if len(points) == 0 {
		return map[string]any{"points": 0}
	}
	var sum, min, max float64
	min, max = points[0].V, points[0].V
	for _, p := range points {
		sum += p.V
		if p.V < min {
			min = p.V
		}
		if p.V > max {
			max = p.V
		}
	}
	return map[string]any{
		"points": len(points),
		"first":  map[string]any{"timestamp": points[0].T.UTC().Format(time.RFC3339), "value": roundSig(points[0].V)},
		"last": map[string]any{
			"timestamp": points[len(points)-1].T.UTC().Format(time.RFC3339),
			"value":     roundSig(points[len(points)-1].V),
		},
		"mean": roundSig(sum / float64(len(points))),
		"min":  roundSig(min),
		"max":  roundSig(max),
	}
}

// seriesJSON renders samples for evidence, keeping only the most recent
// maxSeriesPoints and reporting truncation.
func seriesJSON(points []backend.Point) ([]map[string]any, bool) {
	truncated := false
	pts := points
	if len(pts) > maxSeriesPoints {
		truncated = true
		pts = pts[len(pts)-maxSeriesPoints:]
	}
	out := make([]map[string]any, 0, len(pts))
	for _, p := range pts {
		out = append(out, map[string]any{
			"timestamp": p.T.UTC().Format(time.RFC3339),
			"value":     roundSig(p.V),
		})
	}
	return out, truncated
}

// roundSig rounds to 6 significant digits so JSON output is readable and does
// not carry float noise.
func roundSig(v float64) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	f, err := strconv.ParseFloat(fmt.Sprintf("%.6g", v), 64)
	if err != nil {
		return v
	}
	return f
}
