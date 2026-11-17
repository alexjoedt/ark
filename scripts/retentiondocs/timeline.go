package main

import (
	"cmp"
	"errors"
	"fmt"
	"html"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alexjoedt/ark"
)

// The timelines follow the design picked for the guide: one rule draws a
// time axis with period bands (timelineAxis), several rules draw one lane
// per rule above a lane of what the policy keeps (timelineLanes). Both are
// image files with their own light and dark colours, so GitHub and the
// site show them alike without inline SVG.

// period is a calendar unit forget counts in, cut in the zone of the time
// it is given: forget hands every row over in zone, where the dry run cuts.
type period int

const (
	hour period = iota
	day
	week
	month
	year
)

var periodWords = [...]struct{ noun, adjective string }{
	{"hour", "hourly"}, {"day", "daily"}, {"week", "weekly"}, {"month", "monthly"}, {"year", "yearly"},
}

func (p period) start(t time.Time) time.Time {
	switch p {
	case hour:
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
	case day:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	case week:
		d := day.start(t)
		return d.AddDate(0, 0, -(int(d.Weekday())+6)%7)
	case month:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
	case year:
		return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, t.Location())
	}
	panic(fmt.Sprint("no period ", int(p)))
}

// next returns the start of the period after the one that starts at t.
func (p period) next(t time.Time) time.Time {
	switch p {
	case hour:
		return t.Add(time.Hour)
	case day:
		return t.AddDate(0, 0, 1)
	case week:
		return t.AddDate(0, 0, 7)
	case month:
		return t.AddDate(0, 1, 0)
	case year:
		return t.AddDate(1, 0, 0)
	}
	panic(fmt.Sprint("no period ", int(p)))
}

// labels returns the lines under the band that starts at t, and the coarser
// line shown where it changes.
func (p period) labels(t time.Time) (lines []string, coarse string) {
	switch p {
	case hour:
		return []string{t.Format("15")}, t.Format("Mon 02 Jan 2006")
	case day:
		return []string{t.Format("Mon"), t.Format("02")}, t.Format("Jan 2006")
	case week:
		// The week that holds 1 January may start in December: its number
		// belongs to the ISO year, not to the year of its Monday.
		y, w := t.ISOWeek()
		return []string{"W" + strconv.Itoa(w), t.Format("02 Jan")}, strconv.Itoa(y)
	case month:
		return []string{t.Format("Jan")}, strconv.Itoa(t.Year())
	case year:
		return []string{strconv.Itoa(t.Year())}, ""
	}
	panic(fmt.Sprint("no period ", int(p)))
}

// name returns how the page names the period that starts at t in a history
// of rows: a week by the day it starts.
func (p period) name(t time.Time, rows []row) string {
	_, dayLayout := layouts(rows)
	switch p {
	case hour:
		return t.Format("15:04")
	case day, week:
		return t.Format(dayLayout)
	case month:
		if rows[0].at.Year() != rows[len(rows)-1].at.Year() {
			return t.Format("January 2006")
		}
		return t.Format("January")
	case year:
		return strconv.Itoa(t.Year())
	}
	panic(fmt.Sprint("no period ", int(p)))
}

// rule is one rule of a policy as the timeline draws it.
type rule struct {
	flag   string        // as on the command line, "--keep-daily 3"
	reason string        // the reason ark forget gives, without "oldest "
	count  ark.KeepCount // 0 for a rule without a count
	period period
	cut    bool       // the rule keeps one snapshot per period
	window ark.Within // zero for a rule without a window
}

// rules lists the rules of p in the order ark forget lists their reasons.
func rules(p ark.ForgetPolicy) []rule {
	var list []rule
	for _, tags := range p.KeepTags {
		list = append(list, rule{
			flag:   "--keep-tag " + strings.Join(tags, ","),
			reason: "has tags [" + strings.Join(tags, ", ") + "]",
		})
	}
	if p.KeepWithin != (ark.Within{}) {
		list = append(list, rule{
			flag: "--keep-within " + p.KeepWithin.String(), reason: "within " + p.KeepWithin.String(),
			window: p.KeepWithin,
		})
	}
	if p.KeepLast != 0 {
		list = append(list, rule{
			flag: "--keep-last " + countText(p.KeepLast), reason: "last snapshot", count: p.KeepLast,
		})
	}
	counts := [...]ark.KeepCount{p.KeepHourly, p.KeepDaily, p.KeepWeekly, p.KeepMonthly, p.KeepYearly}
	for i, n := range counts {
		if n != 0 {
			adj := periodWords[i].adjective
			list = append(list, rule{
				flag: "--keep-" + adj + " " + countText(n), reason: adj + " snapshot",
				count: n, period: period(i), cut: true,
			})
		}
	}
	windows := [...]ark.Within{
		p.KeepWithinHourly, p.KeepWithinDaily, p.KeepWithinWeekly, p.KeepWithinMonthly, p.KeepWithinYearly,
	}
	for i, w := range windows {
		if w != (ark.Within{}) {
			adj := periodWords[i].adjective
			list = append(list, rule{
				flag: "--keep-within-" + adj + " " + w.String(), reason: adj + " within " + w.String(),
				period: period(i), cut: true, window: w,
			})
		}
	}
	return list
}

func countText(n ark.KeepCount) string {
	if n == ark.Unlimited {
		return "unlimited"
	}
	return strconv.Itoa(int(n))
}

// ruleMark is what one rule did to the history, with the history's rows.
type ruleMark struct {
	rule
	keeps    map[int]bool   // row indexes of the snapshots it keeps
	pills    map[int]string // row index to the count used, for a counted rule
	newest   int            // row index of the newest snapshot it keeps
	oldest   int            // row index of the oldest
	usedUp   bool           // a counted rule ran out of count
	noCount  []time.Time    // periods it counted across without a snapshot
	winStart time.Time      // of a window rule, whether or not it keeps a snapshot
}

// mark matches the reasons of rows, newest first, to the rules of policy.
// A reason no rule names is an error, so a reason ark changes can never
// drop out of the picture unseen.
func mark(policy ark.ForgetPolicy, rows []row) ([]ruleMark, error) {
	list := rules(policy)
	marks := make([]ruleMark, len(list))
	for i, r := range list {
		marks[i] = ruleMark{rule: r, keeps: map[int]bool{}, pills: map[int]string{}, newest: -1, oldest: -1}
	}
	for ri, row := range rows {
		for _, reason := range row.reasons {
			i := slices.IndexFunc(list, func(r rule) bool {
				return reason == r.reason || reason == "oldest "+r.reason
			})
			if i < 0 {
				return nil, fmt.Errorf("no rule of the policy gives the reason %q", reason)
			}
			m := &marks[i]
			if m.newest < 0 {
				m.newest = ri
			}
			m.oldest = ri
			m.keeps[ri] = true
			if m.count != 0 {
				limit := countText(m.count)
				if m.count == ark.Unlimited {
					limit = "∞"
				}
				m.pills[ri] = strconv.Itoa(len(m.keeps)) + "/" + limit
			}
		}
	}
	for i := range marks {
		m := &marks[i]
		if m.window != (ark.Within{}) {
			// rows[0] is the newest snapshot. One in the future would not
			// move forget's window, and Backup refuses to date one there.
			w := m.window
			m.winStart = rows[0].at.AddDate(-w.Years, -w.Months, -w.Days).Add(-time.Duration(w.Hours) * time.Hour)
		}
		if m.newest < 0 {
			continue
		}
		m.usedUp = m.count > 0 && len(m.keeps) == int(m.count) && m.oldest < len(rows)-1
		if m.count == 0 || !m.cut {
			continue
		}
		taken := map[time.Time]bool{}
		for _, row := range rows {
			taken[m.period.start(row.at)] = true
		}
		last := m.period.start(rows[m.newest].at)
		for p := m.period.start(rows[m.oldest].at); !p.After(last); p = m.period.next(p) {
			if !taken[p] {
				m.noCount = append(m.noCount, p)
			}
		}
	}
	return marks, nil
}

const (
	width    = 720.0
	gap      = 10.0 // between the centres of two snapshots
	dot      = 4.0
	charW    = 7.2 // of the 12px monospace text
	pillH    = 17.0
	rowStep  = 20.0
	maxBands = 40
)

// axis maps times onto x. Bands of one period have equal width; inside a
// band the snapshots sit at their time, spread apart where they would
// overlap, and other times fall between their neighbours.
type axis struct {
	unit    period
	bands   []time.Time // the start of every band and the end of the last
	x0, bw  float64
	snapX   []float64 // per row
	anchors []anchor
}

type anchor struct {
	t time.Time
	x float64
}

// bandUnit picks the finest period that cuts the history into at most
// maxBands bands, or the rule's own period when it does.
func bandUnit(rows []row, own *period) period {
	oldest, newest := rows[len(rows)-1].at, rows[0].at
	fits := func(p period) bool {
		n := 0
		for t := p.start(oldest); !t.After(newest) && n <= maxBands; t = p.next(t) {
			n++
		}
		return n <= maxBands
	}
	if own != nil && fits(*own) {
		return *own
	}
	for p := range year {
		if fits(p) {
			return p
		}
	}
	return year
}

func newAxis(rows []row, unit period, x0, x1 float64) *axis {
	a := &axis{unit: unit, x0: x0}
	for t := unit.start(rows[len(rows)-1].at); !t.After(rows[0].at); t = unit.next(t) {
		a.bands = append(a.bands, t)
	}
	a.bands = append(a.bands, unit.next(a.bands[len(a.bands)-1]))
	a.bw = (x1 - x0) / float64(len(a.bands)-1)
	a.snapX = make([]float64, len(rows))
	for i := range len(a.bands) - 1 {
		lo, hi := a.bands[i], a.bands[i+1]
		bx := x0 + float64(i)*a.bw
		a.anchors = append(a.anchors, anchor{lo, bx})
		var in []int // row indexes, oldest first
		for r, row := range slices.Backward(rows) {
			if !row.at.Before(lo) && row.at.Before(hi) {
				in = append(in, r)
			}
		}
		xs := make([]float64, len(in))
		for j, r := range in {
			xs[j] = bx + a.bw*float64(rows[r].at.Sub(lo))/float64(hi.Sub(lo))
		}
		pad := min(dot+0.5, a.bw/4)
		spread(xs, bx+pad, bx+a.bw-pad)
		for j, r := range in {
			a.snapX[r] = xs[j]
			a.anchors = append(a.anchors, anchor{rows[r].at, xs[j]})
		}
	}
	a.anchors = append(a.anchors, anchor{a.bands[len(a.bands)-1], x1})
	return a
}

// spread moves xs, ascending, apart to gap inside [lo, hi], or spaces them
// evenly when they do not fit.
func spread(xs []float64, lo, hi float64) {
	n := len(xs)
	if n == 0 {
		return
	}
	if float64(n-1)*gap > hi-lo {
		for i := range xs {
			xs[i] = lo + (hi-lo)*(float64(i)+0.5)/float64(n)
		}
		return
	}
	for i := range xs {
		xs[i] = max(xs[i], lo)
		if i > 0 {
			xs[i] = max(xs[i], xs[i-1]+gap)
		}
	}
	for i := n - 1; i >= 0; i-- {
		xs[i] = min(xs[i], hi)
		if i < n-1 {
			xs[i] = min(xs[i], xs[i+1]-gap)
		}
	}
}

// x returns the position of t, clamped to the axis.
func (a *axis) x(t time.Time) float64 {
	i, _ := slices.BinarySearchFunc(a.anchors, t, func(e anchor, t time.Time) int { return e.t.Compare(t) })
	switch {
	case i == 0:
		return a.anchors[0].x
	case i == len(a.anchors):
		return a.anchors[i-1].x
	}
	lo, hi := a.anchors[i-1], a.anchors[i]
	return lo.x + (hi.x-lo.x)*float64(t.Sub(lo.t))/float64(hi.t.Sub(lo.t))
}

// svg collects the elements of one image.
type svg struct{ b strings.Builder }

func num(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }

func (s *svg) addf(format string, args ...any) { fmt.Fprintf(&s.b, format+"\n", args...) }

func (s *svg) text(x, y float64, text, class, anchor string) {
	s.addf(`<text x="%s" y="%s" class="%s" text-anchor="%s">%s</text>`,
		num(x), num(y), class, anchor, html.EscapeString(text))
}

func (s *svg) rect(x, y, w, h float64, class string) {
	s.addf(`<rect x="%s" y="%s" width="%s" height="%s" class="%s"/>`, num(x), num(y), num(max(w, 0)), num(h), class)
}

func (s *svg) circle(x, y float64, class string) {
	s.addf(`<circle cx="%s" cy="%s" r="%s" class="%s"/>`, num(x), num(y), num(dot), class)
}

func keptClass(kept bool) string {
	if kept {
		return "kept"
	}
	return "removed"
}

// pillX returns where the pill of row r centres, inside the axis.
func (a *axis) pillX(r int, text string) float64 {
	w := textWidth(text) + 10
	return min(max(a.snapX[r], a.x0+w/2), a.anchors[len(a.anchors)-1].x-w/2)
}

func (s *svg) pill(x, y float64, text string) {
	w := textWidth(text) + 10
	s.addf(`<rect x="%s" y="%s" width="%s" height="%s" rx="8" class="pill"/>`,
		num(x-w/2), num(y-pillH/2), num(w), num(pillH))
	s.text(x, y+4, text, "on-pill", "middle")
}

func textWidth(s string) float64 { return float64(len([]rune(s))) * charW }

// pillRows assigns the pills of one rule to rows so none overlap, and
// returns each pill's row by row index and the number of rows.
func pillRows(pills map[int]string, a *axis) (map[int]int, int) {
	idx := make([]int, 0, len(pills))
	for r := range pills {
		idx = append(idx, r)
	}
	slices.SortFunc(idx, func(i, j int) int { return cmp.Compare(a.snapX[i], a.snapX[j]) })
	rowOf := map[int]int{}
	var ends []float64
	for _, r := range idx {
		w := textWidth(pills[r]) + 10
		left := a.pillX(r, pills[r]) - w/2
		k := slices.IndexFunc(ends, func(end float64) bool { return end+3 <= left })
		if k < 0 {
			k = len(ends)
			ends = append(ends, 0)
		}
		ends[k] = left + w
		rowOf[r] = k
	}
	return rowOf, len(ends)
}

// bandLabels writes the band labels from y down and returns the y below
// them. A coarse label that would run into the one before it waits for the
// first band clear of it.
func (s *svg) bandLabels(a *axis, y float64) float64 {
	n := len(a.bands) - 1
	widest, lines := 0.0, 0
	for _, t := range a.bands[:n] {
		ls, _ := a.unit.labels(t)
		lines = max(lines, len(ls))
		for _, l := range ls {
			widest = max(widest, textWidth(l))
		}
	}
	step := max(1, int(math.Ceil((widest+4)/a.bw)))
	coarseEnd, prev := math.Inf(-1), ""
	for i, t := range a.bands[:n] {
		bx := a.x0 + float64(i)*a.bw
		ls, coarse := a.unit.labels(t)
		if i%step == 0 {
			for j, l := range ls {
				s.text(bx+a.bw/2, y+13*float64(j+1), l, "muted", "middle")
			}
		}
		if coarse != "" && coarse != prev && bx >= coarseEnd {
			s.text(bx+2, y+13*float64(lines+1), coarse, "", "start")
			coarseEnd = bx + 2 + textWidth(coarse) + 8
			prev = coarse
		}
	}
	if prev != "" {
		lines++
	}
	return y + 13*float64(lines) + 6
}

// hatchPeriods hatches the periods m counted across without a snapshot. A
// period finer than the bands has no band of its own, so a run of them is
// one rectangle: an hourly rule over months of daily backups hatches each
// gap once, not each of its hours.
func (s *svg) hatchPeriods(a *axis, m ruleMark, y, h float64) {
	for i := 0; i < len(m.noCount); i++ {
		from := m.noCount[i]
		for m.period < a.unit && i+1 < len(m.noCount) && m.noCount[i+1].Equal(m.period.next(m.noCount[i])) {
			i++
		}
		x1, x2 := a.x(from), a.x(m.period.next(m.noCount[i]))
		s.rect(x1+1, y, x2-x1-2, h, "hatch")
	}
}

// fadeUsedUp covers what lies before the oldest snapshot a counted rule
// keeps, where it has no count left.
func (s *svg) fadeUsedUp(a *axis, m ruleMark, rows []row, y, h float64) {
	if !m.usedUp {
		return
	}
	end := (a.snapX[m.oldest] + a.snapX[m.oldest+1]) / 2
	if m.cut {
		end = a.x(m.period.start(rows[m.oldest].at))
	}
	s.rect(a.x0, y, end-a.x0, h, "fade")
	if label := "count used up"; end-a.x0 >= textWidth(label)+12 {
		s.text((a.x0+end)/2, y+14, label, "muted", "middle")
	}
}

type legendItem struct{ symbol, text string }

// legend writes the items in rows from y and returns the y below them.
func (s *svg) legend(items []legendItem, x0, y float64) float64 {
	x := x0
	for _, it := range items {
		w := 22 + textWidth(it.text)
		if x > x0 && x+w > width-16 {
			x, y = x0, y+20
		}
		cx, cy := x+7, y-4
		switch it.symbol {
		case "kept", "removed":
			s.circle(cx, cy, it.symbol)
		case "fade":
			s.circle(cx, cy, "removed")
			s.rect(x, cy-6, 14, 12, "fade")
		case "pill":
			s.addf(`<rect x="%s" y="%s" width="14" height="12" rx="6" class="pill"/>`, num(x), num(cy-6))
		default:
			s.rect(x, cy-6, 14, 12, it.symbol)
		}
		s.text(x+20, y, it.text, "", "start")
		x += w + 18
	}
	return y + 10
}

func legendFor(marks []ruleMark) []legendItem {
	items := []legendItem{{"kept", "kept"}, {"removed", "removed"}}
	var counted, hatched, faded, window bool
	for _, m := range marks {
		counted = counted || m.count != 0 && m.newest >= 0
		hatched = hatched || len(m.noCount) > 0
		faded = faded || m.usedUp
		window = window || m.window != (ark.Within{})
	}
	if counted {
		items = append(items, legendItem{"pill", "count used, from the newest"})
	}
	if hatched {
		items = append(items, legendItem{"hatch", "no snapshot, uses no count"})
	}
	if faded {
		items = append(items, legendItem{"fade", "count used up"})
	}
	if window {
		items = append(items, legendItem{"win", "window back from the newest snapshot"})
	}
	return items
}

// timeline draws what policy did to rows, newest first.
func timeline(policy ark.ForgetPolicy, rows []row) ([]byte, error) {
	marks, err := mark(policy, rows)
	if err != nil {
		return nil, err
	}
	if len(marks) == 0 {
		return nil, errors.New("the policy has no rule")
	}
	var body svg
	var height float64
	if len(marks) == 1 {
		height = timelineAxis(&body, marks[0], rows)
	} else {
		height = timelineLanes(&body, marks, rows)
	}
	kept := 0
	for _, r := range rows {
		if r.kept {
			kept++
		}
	}
	flags := make([]string, len(marks))
	for i, m := range marks {
		flags[i] = m.flag
	}
	var out strings.Builder
	fmt.Fprintf(
		&out,
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %s %s" width="%s" height="%s" role="img">`+"\n",
		num(width),
		num(height),
		num(width),
		num(height),
	)
	fmt.Fprintf(
		&out,
		"<title>%s keeps %d of %d snapshots</title>\n",
		html.EscapeString(strings.Join(flags, " ")),
		kept,
		len(rows),
	)
	out.WriteString(style)
	fmt.Fprintf(
		&out,
		`<rect x="0.5" y="0.5" width="%s" height="%s" rx="8" class="bg"/>`+"\n",
		num(width-1),
		num(height-1),
	)
	out.WriteString(body.b.String())
	out.WriteString("</svg>\n")
	return []byte(out.String()), nil
}

// timelineAxis draws one rule on a time axis and returns the height.
func timelineAxis(s *svg, m ruleMark, rows []row) float64 {
	const x0, x1 = 16.0, width - 16
	var own *period
	if m.cut {
		own = &m.period
	}
	a := newAxis(rows, bandUnit(rows, own), x0, x1)
	rowOf, nrows := pillRows(m.pills, a)

	const bandTop = 52.0
	axisY := bandTop + 22 + rowStep*float64(nrows)
	bandBot := axisY + 14
	s.text(x0, 24, m.flag, "", "start")
	timeLayout, _ := layouts(rows)
	switch {
	case m.window != (ark.Within{}):
		s.text(x1, 24, m.window.String()+" window from "+m.winStart.Format(timeLayout), "muted", "end")
	case m.count != 0 && m.newest >= 0:
		what := "snapshot"
		if m.cut {
			what = periodWords[m.period].noun
		}
		s.text(x1, 24, "counted from the newest "+what, "muted", "end")
		if xa, xb := a.snapX[m.oldest], a.snapX[m.newest]; xb-xa > 20 {
			s.addf(`<path d="M %s 38 H %s m 7 -4 l -7 4 l 7 4" class="arrow"/>`, num(xb), num(xa))
		}
	}
	for i := range len(a.bands) - 1 {
		s.rect(x0+float64(i)*a.bw, bandTop, a.bw, bandBot-bandTop, []string{"band", "band2"}[i%2])
	}
	if m.cut {
		s.hatchPeriods(a, m, bandTop, bandBot-bandTop)
	}
	if m.window != (ark.Within{}) {
		xs := a.x(m.winStart)
		s.rect(xs, bandTop-4, a.snapX[0]-xs, bandBot-bandTop+8, "win")
	}
	s.addf(`<line x1="%s" y1="%s" x2="%s" y2="%s" class="axis"/>`, num(x0), num(axisY), num(x1), num(axisY))
	for r, row := range rows {
		s.circle(a.snapX[r], axisY, keptClass(row.kept))
	}
	// The fade covers the removed snapshots, never a pill: a pill may reach
	// past the end of the fade over the snapshot it belongs to.
	s.fadeUsedUp(a, m, rows, bandTop, bandBot-bandTop)
	for r := range rows {
		if text, ok := m.pills[r]; ok {
			x, py := a.snapX[r], axisY-18-rowStep*float64(rowOf[r])
			s.addf(`<line x1="%s" y1="%s" x2="%s" y2="%s" class="lead"/>`, num(x), num(py), num(x), num(axisY-dot))
			s.pill(a.pillX(r, text), py, text)
		}
	}
	y := s.bandLabels(a, bandBot)
	return s.legend(legendFor([]ruleMark{m}), x0, y+18) + 6
}

// timelineLanes draws a lane of snapshots, one lane per rule and a lane of
// what the policy keeps, and returns the height.
func timelineLanes(s *svg, marks []ruleMark, rows []row) float64 {
	const labelW, x1 = 160.0, width - 12
	const laneH, laneGap = 28.0, 6.0
	a := newAxis(rows, bandUnit(rows, nil), labelW, x1)

	type lane struct {
		label  []string
		top, h float64
		mark   *ruleMark
		rowOf  map[int]int
	}
	lanes := []lane{{label: []string{"snapshots"}, h: laneH}}
	for i := range marks {
		m := &marks[i]
		rowOf, n := pillRows(m.pills, a)
		flag, value, _ := strings.Cut(m.flag, " ")
		lanes = append(
			lanes,
			lane{label: []string{flag, value}, h: max(laneH, 12+rowStep*float64(n)), mark: m, rowOf: rowOf},
		)
	}
	lanes = append(lanes, lane{label: []string{"kept"}, h: laneH})
	y := 14.0
	for i := range lanes {
		lanes[i].top = y
		y += lanes[i].h + laneGap
	}
	bottom := y - laneGap

	for _, l := range lanes {
		s.rect(labelW, l.top, x1-labelW, l.h, "lane")
		mid := l.top + l.h/2
		if len(l.label) == 1 {
			s.text(12, mid+4, l.label[0], "", "start")
		} else {
			s.text(12, mid-2, l.label[0], "", "start")
			s.text(12, mid+11, l.label[1], "muted", "start")
		}
	}
	for i := 1; i < len(a.bands)-1; i++ {
		x := labelW + float64(i)*a.bw
		s.addf(`<line x1="%s" y1="%s" x2="%s" y2="%s" class="sep"/>`, num(x), num(lanes[0].top), num(x), num(bottom))
	}
	for _, l := range lanes {
		mid := l.top + l.h/2
		switch {
		case l.mark == nil && l.label[0] == "snapshots":
			for r := range rows {
				s.circle(a.snapX[r], mid, "snap")
			}
		case l.mark == nil:
			for r, row := range rows {
				s.circle(a.snapX[r], mid, keptClass(row.kept))
			}
		default:
			m := l.mark
			if m.cut {
				s.hatchPeriods(a, *m, l.top+2, l.h-4)
			}
			s.fadeUsedUp(a, *m, rows, l.top+2, l.h-4)
			if m.window != (ark.Within{}) {
				xs := a.x(m.winStart)
				s.rect(xs, l.top+2, a.snapX[0]-xs, l.h-4, "win")
			}
			for r := range rows {
				switch text, ok := m.pills[r]; {
				case ok:
					s.pill(a.pillX(r, text), l.top+16+rowStep*float64(l.rowOf[r]), text)
				case m.keeps[r]:
					s.circle(a.snapX[r], mid, "kept")
				}
			}
		}
	}
	y = s.bandLabels(a, bottom)
	return s.legend(legendFor(marks), 12, y+18) + 6
}

// style holds the colours of the docs theme, docs/.vitepress/theme/vault.css,
// light and dark. An image cannot fetch web fonts, so the text is the
// system's monospace.
const style = `<style>
text { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12px; fill: #2c3743; }
.muted { fill: #55626f; }
.bg { fill: #fbfcfd; stroke: #dde3ea; }
.band, .lane { fill: #f3f6f9; }
.band2 { fill: #e9eef3; }
.fade { fill: #fbfcfd; opacity: 0.65; }
.axis, .sep { stroke: #dde3ea; stroke-width: 1.5; }
.kept { fill: #0b6fa8; stroke: #0b6fa8; stroke-width: 1.5; }
.removed { fill: #fbfcfd; stroke: #8794a1; stroke-width: 1.5; }
.snap { fill: #8794a1; }
.pill { fill: #0b6fa8; }
.on-pill { fill: #ffffff; }
.lead { stroke: #0b6fa8; stroke-width: 1; }
.arrow { stroke: #55626f; fill: none; }
.win { fill: #0b6fa8; fill-opacity: 0.1; stroke: #0b6fa8; stroke-width: 1.5; stroke-dasharray: 5 4; }
.hatch { fill: url(#hatch); }
.hatch-line { stroke: #c3ccd6; stroke-width: 1.5; }
@media (prefers-color-scheme: dark) {
  text { fill: #b4c0cd; }
  .muted { fill: #93a0ae; }
  .bg { fill: #0a0e13; stroke: #232b35; }
  .band, .lane { fill: #10161c; }
  .band2 { fill: #161d25; }
  .fade { fill: #0a0e13; }
  .axis, .sep { stroke: #232b35; }
  .kept { fill: #8be4f5; stroke: #8be4f5; }
  .removed { fill: #0a0e13; stroke: #6b7785; }
  .snap { fill: #6b7785; }
  .pill { fill: #8be4f5; }
  .on-pill { fill: #0a0e13; }
  .lead { stroke: #8be4f5; }
  .arrow { stroke: #93a0ae; }
  .win { fill: #8be4f5; stroke: #8be4f5; }
  .hatch-line { stroke: #3a4552; }
}
</style>
<defs><pattern id="hatch" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)"><line x1="0" y1="0" x2="0" y2="6" class="hatch-line"/></pattern></defs>
`
