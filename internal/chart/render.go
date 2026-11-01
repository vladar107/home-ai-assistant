package chart

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"

	"github.com/vladar107/home-ai-assistant/internal/fonts"
)

var (
	background    = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	textPrimary   = color.NRGBA{R: 26, G: 29, B: 35, A: 255}
	textSecondary = color.NRGBA{R: 91, G: 98, B: 112, A: 255}
	gridColour    = color.NRGBA{R: 230, G: 232, B: 236, A: 255}
	axisColour    = color.NRGBA{R: 184, G: 190, B: 199, A: 255}
)

// palette keeps every series colour at least 4.5:1 against the background, so a line, a bar, and
// a marker's value all stay legible, in four clearly different hues; a comparison is also told
// apart by its dashes rather than by colour alone.
var palette = [MaximumSeries]color.NRGBA{
	{R: 36, G: 98, B: 201, A: 255},
	{R: 194, G: 65, B: 12, A: 255},
	{R: 47, G: 125, B: 50, A: 255},
	{R: 162, G: 28, B: 175, A: 255},
}

// The chart is shown about 400 px wide on a phone, a third of its width, so 22 px is the smallest
// text that stays readable there.
const (
	margin = 48

	titleSize      = 38
	legendSize     = 26
	axisSize       = 22
	markerTextSize = 22

	titleBaseline  = 62
	legendBaseline = 110
	unitBaseline   = 150
	plotTop        = 172
	plotBottom     = Height - 66
	timeBaseline   = Height - 28

	axisGap       = 14
	swatchWidth   = 32
	legendSpacing = 36

	lineWidth    = 4
	markerRadius = 7
	markerRing   = 3
	dashLength   = 16
	dashGap      = 10
	barShare     = 0.8
	// lighterBar is the opacity of a comparison bar, which keeps a full-colour outline.
	lighterBar = 0.4
	barOutline = 2
	// minimumBarWidth keeps a bar visible when many share the plot, at the cost of touching.
	minimumBarWidth = 2

	// timeTicks is the most labels the time axis holds before they would run together.
	timeTicks  = 7
	valueTicks = 5
	// maximumValueTicks bounds the tick loop: a nice step gives at most twice valueTicks, plus the
	// headroom steps.
	maximumValueTicks = 2*valueTicks + 3
	// headroom is the share of an axis kept free beyond its extremes.
	headroom = 0.08
)

// Render draws the chart as a Width × Height PNG.
func Render(c Chart) ([]byte, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	set, err := fonts.Inter()
	if err != nil {
		return nil, fmt.Errorf("load chart fonts: %w", err)
	}
	location := c.Location
	if location == nil {
		location = time.UTC
	}
	p := &painter{canvas: image.NewRGBA(image.Rect(0, 0, Width, Height)), fonts: set, faces: map[faceKey]font.Face{}, location: location}
	draw.Draw(p.canvas, p.canvas.Bounds(), image.NewUniform(background), image.Point{}, draw.Src)
	if err := p.draw(c); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := png.Encode(&out, p.canvas); err != nil {
		return nil, fmt.Errorf("encode chart: %w", err)
	}
	return out.Bytes(), nil
}

type painter struct {
	canvas   *image.RGBA
	fonts    fonts.Set
	faces    map[faceKey]font.Face
	location *time.Location
	pending  []markerLabel
	// placed are the boxes of the marker values drawn so far.
	placed []image.Rectangle
	// raster is reused for every shape, sized to the box each one covers.
	raster *vector.Rasterizer
}

type faceKey struct {
	bold bool
	size float64
}

func (p *painter) face(bold bool, size float64) font.Face {
	key := faceKey{bold, size}
	if face, ok := p.faces[key]; ok {
		return face
	}
	source := p.fonts.Regular
	if bold {
		source = p.fonts.SemiBold
	}
	// The fonts are embedded and already parsed, so NewFace can only fail on invalid options.
	face, err := opentype.NewFace(source, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		panic(err)
	}
	p.faces[key] = face
	return face
}

// plot is the area the series are drawn in and the scales that place them there.
type plot struct {
	left, right, top, bottom float64
	times                    timeAxis
	axes                     []valueAxis
	// slot is the width in pixels a bar group may take.
	slot float64
}

func (p *painter) draw(c Chart) error {
	units := c.units()
	axes := make([]valueAxis, len(units))
	for index, unit := range units {
		axis, err := newValueAxis(c, unit)
		if err != nil {
			return err
		}
		axes[index] = axis
	}
	labels := p.face(false, axisSize)
	area := plot{top: plotTop, bottom: plotBottom, times: newTimeAxis(c), axes: axes}
	area.left = float64(margin + axes[0].labelWidth(labels) + axisGap)
	area.right = float64(Width - margin)
	if len(axes) > 1 {
		area.right -= float64(axes[1].labelWidth(labels) + axisGap)
	}
	area.slot = (area.right - area.left) * float64(area.times.slot) / float64(area.times.end.Sub(area.times.start))

	for _, tick := range axes[0].ticks {
		y := math.Round(axes[0].y(tick, area))
		p.fill(image.Rect(int(area.left), int(y), int(area.right), int(y)+1), gridColour)
		label := axes[0].label(tick)
		p.text(labels, label, int(area.left)-axisGap-measure(labels, label), int(y)+capHeight(labels)/2, textSecondary)
	}
	if len(axes) > 1 {
		for _, tick := range axes[1].ticks {
			y := math.Round(axes[1].y(tick, area))
			p.fill(image.Rect(int(area.right), int(y), int(area.right)+6, int(y)+1), axisColour)
			p.text(labels, axes[1].label(tick), int(area.right)+axisGap, int(y)+capHeight(labels)/2, textSecondary)
		}
	}
	p.fill(image.Rect(int(area.left), int(area.bottom), int(area.right), int(area.bottom)+2), axisColour)
	for _, tick := range area.times.ticks(p.location) {
		x := area.x(tick.at)
		p.fill(image.Rect(int(x), int(area.bottom)+2, int(x)+2, int(area.bottom)+10), axisColour)
		width := measure(labels, tick.label)
		left := min(max(int(x)-width/2, margin/2), Width-margin/2-width)
		p.text(labels, tick.label, left, timeBaseline, textSecondary)
	}
	if units[0] != "" {
		p.text(labels, units[0], margin, unitBaseline, textSecondary)
	}
	if len(units) > 1 && units[1] != "" {
		p.text(labels, units[1], Width-margin-measure(labels, units[1]), unitBaseline, textSecondary)
	}

	for index, series := range c.Series {
		axis := axes[slices.Index(units, series.Unit)]
		if c.Kind == Bar {
			p.bars(series, axis, area, palette[index], index, len(c.Series))
		} else {
			p.line(series, axis, area, palette[index], c.Kind == Step)
		}
	}
	for index, series := range c.Series {
		p.markers(c, index, series, axes[slices.Index(units, series.Unit)], area)
	}
	values := p.face(true, markerTextSize)
	for _, pending := range p.pending {
		p.label(values, pending.text, pending.left, pending.baselines, pending.colour)
	}
	p.legend(c)
	title := p.face(true, titleSize)
	p.text(title, fit(title, clean(c.Title), Width-2*margin), margin, titleBaseline, textPrimary)
	return nil
}

func (a plot) x(at time.Time) float64 {
	return a.left + float64(at.Sub(a.times.start))/float64(a.times.end.Sub(a.times.start))*(a.right-a.left)
}

// barOffset is where the bar of series index of count sits from its time, in pixels.
func (a plot) barOffset(index, count int) (offset, width float64) {
	group := a.slot * barShare
	width = math.Max(group/float64(count), minimumBarWidth)
	return -width*float64(count)/2 + width*float64(index), width
}

type vec struct{ x, y float64 }

func (p *painter) line(series Series, axis valueAxis, area plot, colour color.NRGBA, step bool) {
	gaps := gapsAfter(series.Points)
	var shapes [][]vec
	var run []vec
	flush := func() {
		if step {
			run = stepped(run)
		}
		if series.Dashed {
			for _, dash := range dashes(run) {
				shapes = append(shapes, stroke(dash, lineWidth)...)
			}
		} else {
			shapes = append(shapes, stroke(run, lineWidth)...)
		}
		run = nil
	}
	for index, point := range series.Points {
		run = append(run, vec{area.x(point.At), axis.y(point.Value, area)})
		if gaps[index] {
			flush()
		}
	}
	flush()
	p.paint(colour, shapes)
}

func stepped(run []vec) []vec {
	if len(run) < 2 {
		return run
	}
	out := make([]vec, 0, 2*len(run))
	for index, point := range run {
		if index > 0 {
			out = append(out, vec{point.x, run[index-1].y})
		}
		out = append(out, point)
	}
	return out
}

func (p *painter) bars(series Series, axis valueAxis, area plot, colour color.NRGBA, index, count int) {
	offset, width := area.barOffset(index, count)
	base := axis.y(math.Min(math.Max(0, axis.low), axis.high), area)
	var fill, outline [][]vec
	for _, point := range series.Points {
		left := area.x(point.At) + offset
		top := axis.y(point.Value, area)
		low, high := math.Min(top, base), math.Max(top, base)
		fill = append(fill, rectangle(left, low, left+width, high))
		if series.Dashed {
			outline = append(outline,
				rectangle(left, low, left+width, low+barOutline),
				rectangle(left, high-barOutline, left+width, high),
				rectangle(left, low, left+barOutline, high),
				rectangle(left+width-barOutline, low, left+width, high))
		}
	}
	fillColour := colour
	if series.Dashed {
		fillColour.A = uint8(math.Round(255 * lighterBar))
	}
	p.paint(fillColour, fill)
	p.paint(colour, outline)
}

// markers mark the lowest and the highest point with their value, the highest above it and the
// lowest below it, so neither needs a word.
func (p *painter) markers(c Chart, index int, series Series, axis valueAxis, area plot) {
	low, high := series.Points[0], series.Points[0]
	for _, point := range series.Points {
		if point.Value < low.Value {
			low = point
		}
		if point.Value > high.Value {
			high = point
		}
	}
	extremes := []Point{high}
	if low.Value != high.Value {
		extremes = append(extremes, low)
	}
	colour := palette[index]
	face := p.face(true, markerTextSize)
	for order, point := range extremes {
		x := area.x(point.At)
		if c.Kind == Bar {
			offset, width := area.barOffset(index, len(c.Series))
			x += offset + width/2
		}
		y := axis.y(point.Value, area)
		p.paint(background, [][]vec{circle(vec{x, y}, markerRadius+markerRing)})
		p.paint(colour, [][]vec{circle(vec{x, y}, markerRadius)})
		reach := markerRadius + markerRing
		p.placed = append(p.placed, image.Rect(int(x)-reach, int(y)-reach, int(x)+reach, int(y)+reach))

		label := displayNumber(point.Value)
		if series.Unit != "" {
			label += " " + series.Unit
		}
		width := measure(face, label)
		left := min(max(int(x)-width/2, int(area.left)), int(area.right)-width)
		above := int(y) - markerRadius - 10
		below := int(y) + markerRadius + 10 + capHeight(face)
		// A bar's lowest value still sits on top of its bar, so both go above it.
		baselines := []int{above, below}
		if order == 1 && c.Kind != Bar {
			baselines = []int{below, above}
		}
		p.pending = append(p.pending, markerLabel{text: label, left: left, baselines: baselines, colour: colour})
	}
}

// markerLabel is a marker's value, drawn once every marker is, so no marker covers a value.
type markerLabel struct {
	text      string
	left      int
	baselines []int
	colour    color.Color
}

// label draws a marker's value at the first of the baselines, or of the steps beyond them, where
// it stays inside the plot and clear of the values already drawn.
func (p *painter) label(face font.Face, text string, left int, baselines []int, colour color.Color) {
	height := capHeight(face)
	width := measure(face, text)
	step := height + 8
	candidates := slices.Clone(baselines)
	for distance := 1; distance <= 3; distance++ {
		candidates = append(candidates, baselines[0]-distance*step, baselines[0]+distance*step)
	}
	chosen := baselines[0]
	for _, baseline := range candidates {
		box := image.Rect(left-2, baseline-height-2, left+width+2, baseline+2)
		if box.Min.Y < plotTop-12 || box.Max.Y > plotBottom-2 {
			continue
		}
		clear := true
		for _, placed := range p.placed {
			clear = clear && !box.Overlaps(placed)
		}
		if clear {
			chosen = baseline
			break
		}
	}
	p.placed = append(p.placed, image.Rect(left-2, chosen-height-2, left+width+2, chosen+2))
	p.halo(face, text, left, chosen)
	p.text(face, text, left, chosen, colour)
}

func (p *painter) legend(c Chart) {
	face := p.face(false, legendSize)
	count := len(c.Series)
	room := (Width - 2*margin - legendSpacing*(count-1)) / count
	x := margin
	for index, series := range c.Series {
		label := clean(series.Label)
		if series.Unit != "" {
			label += " (" + series.Unit + ")"
		}
		label = fit(face, label, room-swatchWidth-12)
		middle := float64(legendBaseline - capHeight(face)/2)
		var swatch [][]vec
		switch {
		case c.Kind == Bar:
			swatch = [][]vec{rectangle(float64(x), middle-10, float64(x+swatchWidth), middle+10)}
		case series.Dashed:
			for _, dash := range dashes([]vec{{float64(x) + 2, middle}, {float64(x + swatchWidth - 2), middle}}) {
				swatch = append(swatch, stroke(dash, lineWidth)...)
			}
		default:
			swatch = stroke([]vec{{float64(x) + 2, middle}, {float64(x + swatchWidth - 2), middle}}, lineWidth)
		}
		colour := palette[index]
		if c.Kind == Bar && series.Dashed {
			colour.A = uint8(math.Round(255 * lighterBar))
		}
		p.paint(colour, swatch)
		p.text(face, label, x+swatchWidth+12, legendBaseline, textPrimary)
		x += swatchWidth + 12 + measure(face, label) + legendSpacing
	}
}

func (p *painter) text(face font.Face, text string, x, baseline int, colour color.Color) {
	drawer := font.Drawer{Dst: p.canvas, Src: image.NewUniform(colour), Face: face, Dot: fixed.P(x, baseline)}
	drawer.DrawString(text)
}

// halo draws the text in the background colour around where it goes, so a value stays legible
// over a line or a grid line.
func (p *painter) halo(face font.Face, text string, x, baseline int) {
	for _, offset := range []image.Point{{-2, 0}, {2, 0}, {0, -2}, {0, 2}, {-2, -2}, {2, 2}, {-2, 2}, {2, -2}} {
		p.text(face, text, x+offset.X, baseline+offset.Y, background)
	}
}

func (p *painter) fill(area image.Rectangle, colour color.Color) {
	draw.Draw(p.canvas, area, image.NewUniform(colour), image.Point{}, draw.Over)
}

// paint fills the shapes in one colour, rasterizing only the box they cover. Every shape is added
// with the same winding, so where they overlap the rasterizer covers the pixel once rather than
// cancelling it out.
func (p *painter) paint(colour color.Color, shapes [][]vec) {
	low, high := vec{math.Inf(1), math.Inf(1)}, vec{math.Inf(-1), math.Inf(-1)}
	for _, shape := range shapes {
		for _, point := range shape {
			low = vec{math.Min(low.x, point.x), math.Min(low.y, point.y)}
			high = vec{math.Max(high.x, point.x), math.Max(high.y, point.y)}
		}
	}
	if math.IsInf(low.x, 0) {
		return
	}
	box := image.Rect(int(math.Floor(low.x))-1, int(math.Floor(low.y))-1, int(math.Ceil(high.x))+1, int(math.Ceil(high.y))+1).Intersect(p.canvas.Bounds())
	if box.Empty() {
		return
	}
	if p.raster == nil {
		p.raster = vector.NewRasterizer(box.Dx(), box.Dy())
	} else {
		p.raster.Reset(box.Dx(), box.Dy())
	}
	origin := vec{float64(box.Min.X), float64(box.Min.Y)}
	for _, shape := range shapes {
		polygon(p.raster, shape, origin)
	}
	p.raster.DrawOp = draw.Over
	p.raster.Draw(p.canvas, box, image.NewUniform(colour), image.Point{})
}

// stroke is a polyline of the given width as shapes, with round joins and ends.
func stroke(points []vec, width float64) [][]vec {
	var shapes [][]vec
	half := width / 2
	for index := 1; index < len(points); index++ {
		from, to := points[index-1], points[index]
		dx, dy := to.x-from.x, to.y-from.y
		length := math.Hypot(dx, dy)
		if length == 0 {
			continue
		}
		nx, ny := -dy/length*half, dx/length*half
		shapes = append(shapes, []vec{{from.x + nx, from.y + ny}, {to.x + nx, to.y + ny}, {to.x - nx, to.y - ny}, {from.x - nx, from.y - ny}})
	}
	for _, point := range points {
		shapes = append(shapes, circle(point, half))
	}
	return shapes
}

// dashes cuts a polyline into the pieces a dashed line draws.
func dashes(points []vec) [][]vec {
	var pieces [][]vec
	var current []vec
	on, left := true, float64(dashLength)
	if len(points) > 0 {
		current = []vec{points[0]}
	}
	for index := 1; index < len(points); index++ {
		from, to := points[index-1], points[index]
		length := math.Hypot(to.x-from.x, to.y-from.y)
		travelled := 0.0
		for length-travelled > left {
			travelled += left
			share := travelled / length
			cut := vec{from.x + (to.x-from.x)*share, from.y + (to.y-from.y)*share}
			if on {
				pieces = append(pieces, append(current, cut))
				current = nil
				left = dashGap
			} else {
				current = []vec{cut}
				left = dashLength
			}
			on = !on
		}
		left -= length - travelled
		if on {
			current = append(current, to)
		}
	}
	if on && len(current) > 1 {
		pieces = append(pieces, current)
	}
	return pieces
}

func circle(center vec, radius float64) []vec {
	segments := max(12, int(radius*4))
	points := make([]vec, segments)
	for index := range points {
		angle := 2 * math.Pi * float64(index) / float64(segments)
		points[index] = vec{center.x + radius*math.Cos(angle), center.y + radius*math.Sin(angle)}
	}
	return points
}

func rectangle(left, top, right, bottom float64) []vec {
	return []vec{{left, top}, {right, top}, {right, bottom}, {left, bottom}}
}

// polygon adds a closed shape, always clockwise on screen, relative to the rasterizer's origin.
func polygon(rasterizer *vector.Rasterizer, points []vec, origin vec) {
	area := 0.0
	for index, point := range points {
		next := points[(index+1)%len(points)]
		area += point.x*next.y - next.x*point.y
	}
	if area < 0 {
		points = slices.Clone(points)
		slices.Reverse(points)
	}
	rasterizer.MoveTo(float32(points[0].x-origin.x), float32(points[0].y-origin.y))
	for _, point := range points[1:] {
		rasterizer.LineTo(float32(point.x-origin.x), float32(point.y-origin.y))
	}
	rasterizer.ClosePath()
}

// valueAxis is one unit's scale, widened to round ticks.
type valueAxis struct {
	low, high, step float64
	ticks           []float64
	// scale and suffix shorten large numbers, such as 12k for 12000.
	scale    float64
	suffix   string
	decimals int
}

// newValueAxis fails rather than loop or overflow when the values leave no finite, positive span
// to divide, which Chart.check already rules out for values within the chartable magnitudes.
func newValueAxis(c Chart, unit string) (valueAxis, error) {
	low, high := math.Inf(1), math.Inf(-1)
	for _, series := range c.Series {
		if series.Unit != unit {
			continue
		}
		for _, point := range series.Points {
			low, high = math.Min(low, point.Value), math.Max(high, point.Value)
		}
	}
	if c.Kind == Bar {
		low, high = math.Min(low, 0), math.Max(high, 0)
	}
	if low == high {
		pad := math.Abs(high) * 0.1
		if pad == 0 {
			pad = 1
		}
		low, high = low-pad, high+pad
	}
	step := niceStep((high - low) / valueTicks)
	axis := valueAxis{step: step, low: math.Floor(low/step) * step, high: math.Ceil(high/step) * step, scale: 1}
	if !positiveFinite(high-low) || !positiveFinite(step) || !positiveFinite(axis.high-axis.low) || math.IsInf(axis.low, 0) || math.IsInf(axis.high, 0) {
		return valueAxis{}, ErrValueRange
	}
	// Room above the highest point and below the lowest keeps their markers' values inside the
	// plot; bars keep their base at zero.
	if axis.high-high < headroom*(axis.high-axis.low) {
		axis.high += step
	}
	if low-axis.low < headroom*(axis.high-axis.low) && (c.Kind != Bar || low < 0) {
		axis.low -= step
	}
	for index := 0; index <= maximumValueTicks; index++ {
		tick := axis.low + float64(index)*step
		if tick > axis.high+step/2 {
			break
		}
		axis.ticks = append(axis.ticks, tick)
	}
	largest := math.Max(math.Abs(axis.low), math.Abs(axis.high))
	for _, size := range []struct {
		scale  float64
		suffix string
	}{{1e12, "T"}, {1e9, "G"}, {1e6, "M"}, {1e4, "k"}} {
		if largest >= size.scale {
			axis.scale, axis.suffix = size.scale, size.suffix
			if size.suffix == "k" {
				axis.scale = 1e3
			}
			break
		}
	}
	axis.decimals = decimalsOf(step / axis.scale)
	return axis, nil
}

func positiveFinite(value float64) bool {
	return value > 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
}

func niceStep(raw float64) float64 {
	magnitude := math.Pow(10, math.Floor(math.Log10(raw)))
	switch fraction := raw / magnitude; {
	case fraction <= 1:
		return magnitude
	case fraction <= 2:
		return 2 * magnitude
	case fraction <= 2.5:
		return 2.5 * magnitude
	case fraction <= 5:
		return 5 * magnitude
	default:
		return 10 * magnitude
	}
}

func decimalsOf(step float64) int {
	for decimals := 0; decimals < 6; decimals++ {
		scaled := step * math.Pow(10, float64(decimals))
		if math.Abs(scaled-math.Round(scaled)) < 1e-6*math.Max(1, scaled) {
			return decimals
		}
	}
	return 6
}

func (a valueAxis) y(value float64, area plot) float64 {
	return area.bottom - (value-a.low)/(a.high-a.low)*(area.bottom-area.top)
}

func (a valueAxis) label(tick float64) string {
	text := strconv.FormatFloat(tick/a.scale, 'f', a.decimals, 64)
	if strings.Trim(text, "-0.") == "" {
		text = strings.TrimPrefix(text, "-")
	}
	return strings.Replace(text, "-", "−", 1) + a.suffix
}

func (a valueAxis) labelWidth(face font.Face) int {
	widest := 0
	for _, tick := range a.ticks {
		widest = max(widest, measure(face, a.label(tick)))
	}
	return widest
}

// timeAxis spans every drawn point; slot is the smallest spacing between them, which a bar group
// takes.
type timeAxis struct {
	start, end time.Time
	slot       time.Duration
}

func newTimeAxis(c Chart) timeAxis {
	var times []time.Time
	for _, series := range c.Series {
		for _, point := range series.Points {
			times = append(times, point.At)
		}
	}
	slices.SortFunc(times, time.Time.Compare)
	times = slices.CompactFunc(times, time.Time.Equal)
	axis := timeAxis{start: times[0], end: times[len(times)-1]}
	for index := 1; index < len(times); index++ {
		if spacing := times[index].Sub(times[index-1]); axis.slot == 0 || spacing < axis.slot {
			axis.slot = spacing
		}
	}
	if axis.slot == 0 {
		axis.slot = 24 * time.Hour
	}
	if c.Kind == Bar || axis.start.Equal(axis.end) {
		axis.start, axis.end = axis.start.Add(-axis.slot/2), axis.end.Add(axis.slot/2)
	}
	return axis
}

type timeTick struct {
	at    time.Time
	label string
}

// tickRule is a spacing of time labels: a whole number of minutes or days, or of months.
type tickRule struct {
	minutes, days, months int
}

var tickRules = []tickRule{
	{minutes: 1}, {minutes: 5}, {minutes: 15}, {minutes: 30},
	{minutes: 60}, {minutes: 120}, {minutes: 180}, {minutes: 360}, {minutes: 720},
	{days: 1}, {days: 2}, {days: 7}, {days: 14},
	{months: 1}, {months: 2}, {months: 3}, {months: 6}, {months: 12}, {months: 24}, {months: 60},
}

func (r tickRule) approximate() time.Duration {
	return time.Duration(r.minutes)*time.Minute + time.Duration(r.days)*24*time.Hour + time.Duration(r.months)*30*24*time.Hour
}

// ticks are the time labels: local clock times within a day, dates across days, and months or
// years beyond, all written with numbers so no language is assumed.
func (a timeAxis) ticks(location *time.Location) []timeTick {
	span := a.end.Sub(a.start)
	rule := tickRules[len(tickRules)-1]
	for _, candidate := range tickRules {
		if span/candidate.approximate() <= timeTicks {
			rule = candidate
			break
		}
	}
	start := a.start.In(location)
	var at func(index int) time.Time
	switch {
	case rule.minutes > 0:
		day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, location)
		at = func(index int) time.Time { return day.Add(time.Duration(index*rule.minutes) * time.Minute) }
	case rule.days > 0:
		first := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, location)
		if rule.days >= 7 {
			first = first.AddDate(0, 0, -((int(first.Weekday()) + 6) % 7))
		}
		at = func(index int) time.Time { return first.AddDate(0, 0, index*rule.days) }
	default:
		year, month := start.Year(), int(start.Month())-1
		month -= month % min(rule.months, 12)
		at = func(index int) time.Time {
			return time.Date(year, time.Month(month+1+index*rule.months), 1, 0, 0, 0, 0, location)
		}
	}
	var ticks []timeTick
	for index := 0; len(ticks) <= 2*timeTicks; index++ {
		tick := at(index)
		if tick.After(a.end) {
			break
		}
		if tick.Before(a.start) {
			continue
		}
		ticks = append(ticks, timeTick{at: tick, label: tickLabel(tick, rule, span)})
	}
	return ticks
}

func tickLabel(at time.Time, rule tickRule, span time.Duration) string {
	switch {
	case rule.minutes > 0 && (span <= 24*time.Hour || at.Hour() != 0 || at.Minute() != 0):
		return at.Format("15:04")
	case rule.minutes > 0, rule.days > 0:
		return at.Format("02.01")
	case rule.months < 12:
		return at.Format("01.2006")
	default:
		return at.Format("2006")
	}
}

func measure(face font.Face, text string) int {
	return font.MeasureString(face, text).Ceil()
}

func capHeight(face font.Face) int {
	return face.Metrics().CapHeight.Round()
}

func clean(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// fit returns text unchanged when it fits in width pixels, and otherwise the longest prefix that
// fits with an ellipsis appended.
func fit(face font.Face, text string, width int) string {
	if measure(face, text) <= width {
		return text
	}
	runes := []rune(text)
	for length := len(runes) - 1; length > 0; length-- {
		candidate := strings.TrimRight(string(runes[:length]), " ·,.;:-–—(") + "…"
		if measure(face, candidate) <= width {
			return candidate
		}
	}
	return "…"
}

// displayNumber writes a value for the image, with a true minus sign, which reads better than a
// hyphen at display sizes.
func displayNumber(value float64) string {
	return strings.Replace(formatNumber(value), "-", "−", 1)
}

// formatNumber keeps about four significant digits for the value's size.
func formatNumber(value float64) string {
	decimals := 0
	switch magnitude := math.Abs(value); {
	case magnitude < 10:
		decimals = 3
	case magnitude < 100:
		decimals = 2
	case magnitude < 10000:
		decimals = 1
	}
	text := strconv.FormatFloat(value, 'f', decimals, 64)
	if strings.Contains(text, ".") {
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	}
	if text == "-0" {
		text = "0"
	}
	return text
}
