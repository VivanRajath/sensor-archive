package sim

import (
	"math"
	"sort"
)

type climate struct {
	name, label                 string
	base, amp, phase, noise, hum float64
}

// Twelve stations in six climate pairs, so similarity search has a right answer.
var climates = []climate{
	{"A", "Coastal", 18, 3, 0, 0.6, 78},
	{"B", "Desert", 30, 11, 1, 0.8, 18},
	{"C", "Mountain", 4, 6, -1, 0.9, 65},
	{"D", "Tropical", 28, 3.5, 0.5, 0.5, 85},
	{"E", "Continental", 15, 10, 1, 0.9, 42},
	{"F", "Polar", -18, 2.5, 0, 0.7, 70},
	{"G", "Coastal", 17.5, 3.2, 0.2, 0.6, 80},
	{"H", "Desert", 31, 11.5, 1.1, 0.8, 16},
	{"I", "Mountain", 4.5, 6.3, -0.8, 0.9, 63},
	{"J", "Tropical", 27.5, 3.4, 0.6, 0.5, 86},
	{"K", "Continental", 14.5, 9.6, 1.2, 0.9, 45},
	{"L", "Polar", -17, 2.8, 0.2, 0.7, 72},
}

const SimilarityWindow = 2 * DayTicks

// shapeWeight scales the 24 hourly-shape features so that, as a group, they
// count about as much as the 7 level features instead of outvoting them.
const shapeWeight = 0.55

// Match is one result of a similarity query.
type Match struct {
	Station  string  `json:"station"`
	Climate  string  `json:"climate"`
	Score    float64 `json:"score"`    // 1 = identical
	Distance float64 `json:"distance"` // Euclidean, in normalised units
	Coverage float64 `json:"coverage"`
	Excluded bool    `json:"excluded"`
	Reason   string  `json:"reason,omitempty"`
}

// SimilarResult explains the query as well as answering it.
type SimilarResult struct {
	Target      string             `json:"target"`
	Features    []string           `json:"features"`
	TargetVec   []float64          `json:"targetVec"`
	Matches     []Match            `json:"matches"`
	Dims        int                `json:"dims"`
	MemoryBytes int                `json:"memoryBytes"`
	Ops         int                `json:"ops"`
	Profiles    map[string][]float64 `json:"profiles"`
}

func featureNames() []string {
	names := []string{"mean", "std", "min", "max", "daily swing", "humidity", "coverage"}
	for h := 0; h < 24; h++ {
		names = append(names, "hour "+itoa2(h))
	}
	return names
}

func itoa2(h int) string { return string([]byte{byte('0' + h/10), byte('0' + h%10)}) }

// features returns a 31-number vector over the last 2 days, plus coverage.
func (s *Sim) features(station string) ([]float64, []float64, float64) {
	from := s.Now - SimilarityWindow
	var sum, sq, hsum float64
	mn, mx := math.Inf(1), math.Inf(-1)
	hourSum := make([]float64, 24)
	hourN := make([]float64, 24)
	n := 0.0
	for _, r := range s.Store.Rows {
		if r.Station != station || r.Dup || r.EventTime < from || r.EventTime > s.Now {
			continue
		}
		c := r.Current()
		t := c.TempC
		sum += t
		sq += t * t
		hsum += c.Hum
		mn = math.Min(mn, t)
		mx = math.Max(mx, t)
		h := (r.EventTime % DayTicks) / 60
		hourSum[h] += t
		hourN[h]++
		n++
	}
	st := s.byName[station]
	expected := float64(SimilarityWindow / st.Interval)
	cov := n / expected
	if n == 0 {
		return nil, nil, 0
	}
	mean := sum / n
	std := math.Sqrt(math.Max(0, sq/n-mean*mean))
	prof := make([]float64, 24)
	pmn, pmx := math.Inf(1), math.Inf(-1)
	for h := range prof {
		if hourN[h] > 0 {
			prof[h] = hourSum[h] / hourN[h]
		} else {
			prof[h] = mean
		}
		pmn = math.Min(pmn, prof[h])
		pmx = math.Max(pmx, prof[h])
	}
	vec := []float64{mean, std, mn, mx, pmx - pmn, hsum / n, cov}
	for _, p := range prof {
		vec = append(vec, p-mean) // shape of the day, independent of level
	}
	return vec, prof, cov
}

// Similar ranks every station by Euclidean distance between z-normalised features.
func (s *Sim) Similar(target string) SimilarResult {
	res := SimilarResult{Target: target, Features: featureNames(), Profiles: map[string][]float64{}}
	vecs := map[string][]float64{}
	covs := map[string]float64{}
	var names []string
	for _, st := range s.Stations {
		v, prof, cov := s.features(st.Name)
		covs[st.Name] = cov
		if prof != nil {
			for i := range prof {
				prof[i] = round1(prof[i])
			}
			res.Profiles[st.Name] = prof
		}
		if v != nil && cov >= 0.7 {
			vecs[st.Name] = v
			names = append(names, st.Name)
		}
	}
	if len(names) == 0 {
		return res
	}
	dims := len(vecs[names[0]])
	res.Dims = dims
	res.MemoryBytes = len(s.Stations) * dims * 4
	// Level features (mean, spread, min, max, swing, humidity): z-normalise each
	// across stations. Coverage (dim 6) stays raw 0–1: z-scoring it would turn
	// meaningless 1.00-vs-0.99 differences into whole standard deviations.
	for d := 0; d < 6; d++ {
		var m, v float64
		for _, n := range names {
			m += vecs[n][d]
		}
		m /= float64(len(names))
		for _, n := range names {
			v += (vecs[n][d] - m) * (vecs[n][d] - m)
		}
		sd := math.Sqrt(v / float64(len(names)))
		for _, n := range names {
			if sd > 1e-9 {
				vecs[n][d] = (vecs[n][d] - m) / sd
			} else {
				vecs[n][d] = 0
			}
		}
	}
	// Shape features (24 hourly deviations): scale as one group by their pooled
	// spread. Normalising each hour separately would blow up noise at hours where
	// every station happens to sit near its daily mean.
	var pooled float64
	cnt := 0
	for _, n := range names {
		for d := 7; d < dims; d++ {
			pooled += vecs[n][d] * vecs[n][d]
			cnt++
		}
	}
	if pooled = math.Sqrt(pooled / float64(cnt)); pooled < 1e-9 {
		pooled = 1
	}
	for _, n := range names {
		for d := 7; d < dims; d++ {
			vecs[n][d] = vecs[n][d] / pooled * shapeWeight
		}
	}
	tv, ok := vecs[target]
	if ok {
		res.TargetVec = tv
	}
	for _, st := range s.Stations {
		m := Match{Station: st.Name, Climate: st.Climate, Coverage: round2(covs[st.Name])}
		if st.Name == target {
			continue
		}
		v, has := vecs[st.Name]
		switch {
		case !ok:
			m.Excluded, m.Reason = true, "target has under 70% coverage"
		case !has:
			m.Excluded, m.Reason = true, "under 70% data coverage in the window"
		default:
			m.Distance = round2(euclid(tv, v))
			m.Score = round2(1 / (1 + m.Distance/math.Sqrt(float64(dims))))
			res.Ops += dims
		}
		res.Matches = append(res.Matches, m)
	}
	sort.SliceStable(res.Matches, func(i, j int) bool {
		if res.Matches[i].Excluded != res.Matches[j].Excluded {
			return !res.Matches[i].Excluded
		}
		return res.Matches[i].Score > res.Matches[j].Score
	})
	s.Stats.ReadsServed++
	return res
}

func euclid(a, b []float64) float64 {
	var d float64
	for i := range a {
		d += (a[i] - b[i]) * (a[i] - b[i])
	}
	return math.Sqrt(d)
}

func cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
