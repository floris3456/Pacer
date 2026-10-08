package capacity

import (
	"encoding/json"
	"sort"
	"time"
)

// HistoryLimit bounds the earlier intervals reported beside the newest one.
const HistoryLimit = 8

// View is the readback: the newest judged interval with its age, and earlier
// intervals beside it. Values are never averaged or carried without their
// interval. There is no field for a known capacity or a declared size.
type View struct {
	Status     string
	Reasons    []string
	Window     string
	From, To   *time.Time
	AgeSeconds *int64
	Value      *Value
	Basis      string
	History    []Interval
}

// NewView orders intervals newest first (longer window first on ties). With
// no interval it is unknown for the given reasons.
func NewView(intervals []Interval, now time.Time, reasons ...string) View {
	if len(intervals) == 0 {
		v := View{Status: "unknown", Reasons: unique(reasons), History: []Interval{}}
		if len(v.Reasons) == 0 {
			v.Reasons = []string{"no_judged_interval"}
		}
		return v
	}
	sorted := append([]Interval(nil), intervals...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].To.Equal(sorted[j].To) {
			return sorted[i].To.After(sorted[j].To)
		}
		return sorted[i].seconds > sorted[j].seconds
	})
	newest := sorted[0]
	from, to := newest.From, newest.To
	age := int64(now.Sub(to) / time.Second)
	if age < 0 {
		age = 0
	}
	v := View{Status: newest.Status, Reasons: newest.Reasons, Window: newest.Window, From: &from, To: &to, AgeSeconds: &age, Value: newest.Value, Basis: newest.Basis}
	v.History = append([]Interval{}, sorted[1:min(len(sorted), 1+HistoryLimit)]...)
	return v
}

// MarshalJSON always reports exclusivity unproven and capacity_known false.
func (v View) MarshalJSON() ([]byte, error) {
	reasons, history := v.Reasons, v.History
	if reasons == nil {
		reasons = []string{}
	}
	if history == nil {
		history = []Interval{}
	}
	return json.Marshal(struct {
		Status        string     `json:"status"`
		Reasons       []string   `json:"reasons"`
		Exclusivity   string     `json:"exclusivity"`
		CapacityKnown bool       `json:"capacity_known"`
		Window        string     `json:"window,omitempty"`
		From          *time.Time `json:"interval_from"`
		To            *time.Time `json:"interval_to"`
		AgeSeconds    *int64     `json:"age_seconds"`
		Value         *Value     `json:"value"`
		Basis         string     `json:"basis,omitempty"`
		History       []Interval `json:"history"`
	}{v.Status, reasons, "unproven", false, v.Window, v.From, v.To, v.AgeSeconds, v.Value, v.Basis, history})
}
