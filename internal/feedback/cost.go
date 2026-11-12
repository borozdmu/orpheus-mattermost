// Package feedback formats a message-level model-cost estimate from recorded
// usage. It never treats absent usage as zero or reasoning as extra output.
package feedback

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Usage struct{ Input, Cached, Output, Total int64 }
type Price struct {
	Input  float64 `json:"input"`
	Cached float64 `json:"cached"`
	Output float64 `json:"output"`
}
type Basis struct {
	FX     map[string]float64 `json:"fx"`
	Models map[string]Price   `json:"models"`
}

func Enabled(created time.Time) bool {
	since, err := strconv.ParseInt(os.Getenv("BOT_FEEDBACK_SINCE"), 10, 64)
	return err == nil && since > 0 && !created.Before(time.Unix(since, 0))
}

func Footer(model string, u Usage, created time.Time) string {
	if !Enabled(created) {
		return ""
	}
	var b Basis
	raw, err := os.ReadFile(os.Getenv("BOT_COST_FILE"))
	if err != nil || json.Unmarshal(raw, &b) != nil {
		return unavailable()
	}
	return Format(model, u, created, b)
}

func unavailable() string {
	return "Стоимость обработки: недоступна — нет подтверждённых данных о расходе или тарифе."
}

// A dated FX entry is immutable. Reconciliation therefore recreates exactly the
// same footer after midnight or a restart, without conflicting with receipts.
func Format(model string, u Usage, created time.Time, b Basis) string {
	if u.Total <= 0 || u.Input < 0 || u.Cached < 0 || u.Cached > u.Input || u.Output < 0 {
		return unavailable()
	}
	day := created.In(time.FixedZone("MSK", 3*3600)).Format(time.DateOnly)
	var dates []string
	for k := range b.FX {
		if k <= day {
			dates = append(dates, k)
		}
	}
	slices.Sort(dates)
	if len(dates) == 0 {
		return unavailable()
	}
	date := dates[len(dates)-1]
	d, err := time.Parse(time.DateOnly, date)
	requested, _ := time.Parse(time.DateOnly, day)
	p, ok := b.Models[model]
	rate := b.FX[date]
	if err != nil || requested.Sub(d) > 7*24*time.Hour || !ok || rate <= 0 || p.Input <= 0 || p.Output <= 0 || p.Cached < 0 {
		return unavailable()
	}
	rub := (float64(u.Input-u.Cached)*p.Input + float64(u.Cached)*p.Cached + float64(u.Output)*p.Output) / 1e6 * rate
	if math.IsNaN(rub) || math.IsInf(rub, 0) || rub < 0 {
		return unavailable()
	}
	amount := strings.ReplaceAll(fmt.Sprintf("%.2f", rub), ".", ",")
	if rub > 0 && rub < 0.01 {
		amount = "<0,01"
	}
	return "Стоимость обработки: ≈" + amount + " ₽"
}
