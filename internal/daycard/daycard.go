// Package daycard draws the day card: a weather-widget style picture with the header forecast,
// one tile per leg of the day, and optional train status rows. It is language-neutral; every word
// on the card comes from the caller, and the renderer only formats temperatures.
package daycard

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strconv"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

const (
	// Width is the card's width in pixels; its height follows the content.
	Width = 1080

	MaximumLegs   = 4
	MaximumTrains = 3
)

var (
	ErrLegCount   = errors.New("a day card needs 1 to " + strconv.Itoa(MaximumLegs) + " legs")
	ErrTrainCount = errors.New("a day card holds at most " + strconv.Itoa(MaximumTrains) + " trains")
)

// Card is the content of one day card.
type Card struct {
	// Title is the top line, such as "Пн 12.10 · Amersfoort Schothorst → Utrecht".
	Title string
	// HeaderLabel says where and when the header forecast applies, such as "Amersfoort Schothorst · 07:00".
	HeaderLabel    string
	Icon           Condition
	Night          bool
	Temperature    float64
	FeelsLike      float64
	FeelsLikeLabel string
	// Summary is one sentence about the day; it wraps to at most two lines.
	Summary string
	Legs    []Leg
	// Trains are drawn as one status row each below the legs; there may be none.
	Trains []Train
}

// TrainStatus is the colour of a train row's status pill.
type TrainStatus int

const (
	TrainOnTime TrainStatus = iota
	TrainSlower
	TrainDisrupted
)

// Train is one travel direction's status, such as "Туда" with "+12 мин" and "08:10 → 09:02".
type Train struct {
	Label  string
	Status TrainStatus
	Badge  string
	Times  string
}

// Leg is one tile, such as a place and its hours.
type Leg struct {
	// Label is the place and its hours, such as "Amersfoort Schothorst · 07–09"; the part after the last " · "
	// goes on the tile's second line.
	Label       string
	Icon        Condition
	Night       bool
	Temperature float64
	// Detail is the tile's bottom text, such as "3.4 мм · порывы 47" or "dry"; it wraps to at most
	// two lines.
	Detail string
}

// Render draws the card as a PNG that is Width pixels wide and as tall as its content. Text that
// does not fit is cut with an ellipsis.
func Render(card Card) ([]byte, error) {
	if len(card.Legs) == 0 || len(card.Legs) > MaximumLegs {
		return nil, ErrLegCount
	}
	if len(card.Trains) > MaximumTrains {
		return nil, ErrTrainCount
	}
	if err := checkReading(card.Icon, card.Temperature, card.FeelsLike); err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	for index, leg := range card.Legs {
		if err := checkReading(leg.Icon, leg.Temperature, 0); err != nil {
			return nil, fmt.Errorf("leg %d: %w", index+1, err)
		}
	}
	for index, train := range card.Trains {
		if train.Status < TrainOnTime || train.Status > TrainDisrupted {
			return nil, fmt.Errorf("train %d: unknown status %d", index+1, int(train.Status))
		}
	}
	set, err := loadAssets()
	if err != nil {
		return nil, fmt.Errorf("load day card assets: %w", err)
	}

	p := newPainter(set)
	layout := p.layout(card)
	p.canvas = image.NewRGBA(image.Rect(0, 0, Width, layout.height))
	paintBackground(p.canvas, paletteFor(card.Icon, card.Night))
	p.draw(card, layout)

	var out bytes.Buffer
	if err := png.Encode(&out, p.canvas); err != nil {
		return nil, fmt.Errorf("encode day card: %w", err)
	}
	return out.Bytes(), nil
}

func checkReading(condition Condition, temperatures ...float64) error {
	if !condition.valid() {
		return fmt.Errorf("unknown condition %d", int(condition))
	}
	for _, value := range temperatures {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("temperature is not a number")
		}
	}
	return nil
}

var (
	textPrimary   = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	textSecondary = color.NRGBA{R: 255, G: 255, B: 255, A: 215}
	panelFill     = color.NRGBA{R: 255, G: 255, B: 255, A: 36}
	dividerFill   = color.NRGBA{R: 255, G: 255, B: 255, A: 72}
	textDark      = color.NRGBA{R: 43, G: 30, B: 0, A: 255}
)

// Pill colours keep at least 3:1 contrast with their semibold text, the WCAG bar for large text.
var trainColours = [...]struct{ fill, text color.NRGBA }{
	TrainOnTime:    {color.NRGBA{R: 30, G: 142, B: 70, A: 255}, textPrimary},
	TrainSlower:    {color.NRGBA{R: 245, G: 184, B: 46, A: 255}, textDark},
	TrainDisrupted: {color.NRGBA{R: 214, G: 54, B: 54, A: 255}, textPrimary},
}

// The card is shown about 400 px wide on a phone, so 36 px is the smallest text that stays
// readable there.
const (
	margin = 56

	titleSize       = 40
	temperatureSize = 170
	sideSize        = 40
	feelsLikeSize   = 64
	summarySize     = 48
	tileTextSize    = 36
	tileTempSize    = 60
	trainBadgeSize  = 36
	trainLabelSize  = 40
	trainTimesSize  = 40

	titleBaseline       = 84
	temperatureBaseline = 262
	summaryLineHeight   = 60
	tileLineHeight      = 46

	headerIconCenterX = margin + 100
	headerTextLeft    = 300

	panelLeft    = 24
	panelRight   = Width - 24
	panelRadius  = 36
	panelPadding = 36
	tilePadding  = 14

	trainGap     = 16
	trainHeight  = 100
	trainRadius  = 30
	trainPadding = 20
	pillHeight   = 64
	pillPadding  = 22

	bottomPadding = 28
)

type painter struct {
	canvas *image.RGBA
	set    *assetSet
	faces  map[faceKey]font.Face
}

type faceKey struct {
	bold bool
	size float64
}

func newPainter(set *assetSet) *painter {
	return &painter{set: set, faces: map[faceKey]font.Face{}}
}

func (p *painter) face(bold bool, size float64) font.Face {
	key := faceKey{bold, size}
	if face, ok := p.faces[key]; ok {
		return face
	}
	source := p.set.regular
	if bold {
		source = p.set.semibold
	}
	// Both fonts are embedded and already parsed, so NewFace can only fail on invalid options.
	face, err := opentype.NewFace(source, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		panic(err)
	}
	p.faces[key] = face
	return face
}

// cardLayout holds everything that decides the card's height, measured before the canvas exists.
type cardLayout struct {
	summary     []string
	tiles       []tileText
	labelLines  int
	detailLines int
	panelTop    int
	panelBottom int
	height      int
}

type tileText struct {
	place, hours string
	detail       []string
}

func (p *painter) layout(card Card) cardLayout {
	var result cardLayout
	summary := p.face(false, summarySize)
	result.summary = wrap(summary, clean(card.Summary), Width-2*margin, 2)

	text := p.face(false, tileTextSize)
	inner := (panelRight-panelLeft)/len(card.Legs) - 2*tilePadding
	result.labelLines, result.detailLines = 1, 1
	for _, leg := range card.Legs {
		place, hours := splitLabel(clean(leg.Label))
		tile := tileText{place: fit(text, place, inner), hours: fit(text, hours, inner)}
		tile.detail = wrapDetail(text, clean(leg.Detail), inner)
		if hours != "" {
			result.labelLines = 2
		}
		result.detailLines = max(result.detailLines, len(tile.detail))
		result.tiles = append(result.tiles, tile)
	}

	result.panelTop = temperatureBaseline + 56
	if len(result.summary) > 0 {
		result.panelTop = temperatureBaseline + 92 + (len(result.summary)-1)*summaryLineHeight + 48
	}
	result.panelBottom = result.panelTop + p.panelHeight(result)
	result.height = result.panelBottom + len(card.Trains)*(trainGap+trainHeight) + bottomPadding
	return result
}

func (p *painter) panelHeight(layout cardLayout) int {
	return valueOffset(layout.labelLines) + detailOffset(p.face(false, tileTextSize)) + (layout.detailLines-1)*tileLineHeight + panelPadding
}

func valueOffset(labelLines int) int {
	return panelPadding + labelLines*tileLineHeight + 72
}

func detailOffset(text font.Face) int {
	return 36 + capHeight(text)
}

// fitLabel cuts the place rather than the time in a label such as "Amersfoort Schothorst · 07:00".
func fitLabel(face font.Face, label string, width int) string {
	place, hours := splitLabel(label)
	if hours == "" || measure(face, label) <= width {
		return fit(face, label, width)
	}
	suffix := " · " + hours
	if place = fit(face, place, width-measure(face, suffix)); place == "" {
		return fit(face, label, width)
	}
	return place + suffix
}

// splitLabel separates "Amersfoort Schothorst · 07–09" into the place and the hours.
func splitLabel(label string) (place, hours string) {
	index := strings.LastIndex(label, " · ")
	if index < 0 {
		return label, ""
	}
	return label[:index], label[index+len(" · "):]
}

func (p *painter) draw(card Card, layout cardLayout) {
	right := Width - margin

	title := p.face(false, titleSize)
	p.text(title, fit(title, clean(card.Title), right-margin), margin, titleBaseline, textPrimary)

	big := p.face(false, temperatureSize)
	capTop := temperatureBaseline - capHeight(big)
	p.icon(p.set.icon(p.set.large, card.Icon, card.Night), headerIconCenterX, capTop+capHeight(big)/2)

	temperature := degrees(card.Temperature)
	p.text(big, temperature, headerTextLeft, temperatureBaseline, textPrimary)

	sideLeft := headerTextLeft + measure(big, temperature) + 32
	sideWidth := right - sideLeft
	side := p.face(false, sideSize)
	feels := p.face(true, feelsLikeSize)
	p.text(side, fitLabel(side, clean(card.HeaderLabel), sideWidth), sideLeft, capTop+capHeight(side), textSecondary)
	feelsValue := degrees(card.FeelsLike)
	feelsLabel := clean(card.FeelsLikeLabel)
	if feelsLabel != "" {
		feelsLabel = fit(side, feelsLabel, sideWidth-measure(feels, feelsValue)-14)
	}
	if feelsLabel != "" {
		p.text(side, feelsLabel, sideLeft, temperatureBaseline, textSecondary)
		sideLeft += measure(side, feelsLabel) + 14
	}
	p.text(feels, feelsValue, sideLeft, temperatureBaseline, textPrimary)

	summary := p.face(false, summarySize)
	for index, line := range layout.summary {
		p.text(summary, line, margin, temperatureBaseline+92+index*summaryLineHeight, textPrimary)
	}

	p.panel(card.Legs, layout)
	// One pill width for every row keeps the labels in a column.
	badge := p.face(true, trainBadgeSize)
	badgeLimit := (panelRight-panelLeft-2*trainPadding)/3 - 2*pillPadding
	badgeWidth := 0
	for _, train := range card.Trains {
		badgeWidth = max(badgeWidth, measure(badge, fit(badge, clean(train.Badge), badgeLimit)))
	}
	top := layout.panelBottom
	for _, train := range card.Trains {
		top += trainGap
		p.train(train, top, badgeWidth)
		top += trainHeight
	}
}

func (p *painter) panel(legs []Leg, layout cardLayout) {
	top, bottom := layout.panelTop, layout.panelBottom
	roundedRect(p.canvas, panelLeft, top, panelRight, bottom, panelRadius, panelFill)

	text := p.face(false, tileTextSize)
	temp := p.face(true, tileTempSize)
	tileWidth := float64(panelRight-panelLeft) / float64(len(legs))
	labelBaseline := top + panelPadding + capHeight(text)
	valueBaseline := top + valueOffset(layout.labelLines)
	detailBaseline := valueBaseline + detailOffset(text)
	for index, leg := range legs {
		tile := layout.tiles[index]
		left := panelLeft + int(math.Round(float64(index)*tileWidth))
		width := int(math.Round(float64(index+1)*tileWidth)) - (left - panelLeft)
		center := left + width/2

		if index > 0 {
			fill(p.canvas, image.Rect(left-1, top+32, left+1, bottom-32), dividerFill)
		}

		p.centered(text, tile.place, center, labelBaseline, textSecondary)
		p.centered(text, tile.hours, center, labelBaseline+tileLineHeight, textSecondary)

		icon := p.set.icon(p.set.small, leg.Icon, leg.Night)
		value := degrees(leg.Temperature)
		const gap = 14
		group := icon.Bounds().Dx() + gap + measure(temp, value)
		groupLeft := center - group/2
		p.icon(icon, groupLeft+icon.Bounds().Dx()/2, valueBaseline-capHeight(temp)/2)
		p.text(temp, value, groupLeft+icon.Bounds().Dx()+gap, valueBaseline, textPrimary)

		for line, detail := range tile.detail {
			p.centered(text, detail, center, detailBaseline+line*tileLineHeight, textSecondary)
		}
	}
}

func (p *painter) train(train Train, top, badgeWidth int) {
	roundedRect(p.canvas, panelLeft, top, panelRight, top+trainHeight, trainRadius, panelFill)
	left, right := panelLeft+trainPadding, panelRight-trainPadding-8
	centerY := top + trainHeight/2

	badge := p.face(true, trainBadgeSize)
	colours := trainColours[train.Status]
	badgeText := fit(badge, clean(train.Badge), badgeWidth)
	pillWidth := badgeWidth + 2*pillPadding
	roundedRect(p.canvas, left, centerY-pillHeight/2, left+pillWidth, centerY+pillHeight/2, pillHeight/2, colours.fill)
	p.centered(badge, badgeText, left+pillWidth/2, centerY+capHeight(badge)/2, colours.text)

	label := p.face(true, trainLabelSize)
	times := p.face(false, trainTimesSize)
	const gap = 24
	labelLeft := left + pillWidth + gap
	available := right - labelLeft
	labelText, timesText := clean(train.Label), clean(train.Times)
	// Times give way first, but keep a whole "08:10 → 09:02" before the label is cut.
	minimumTimes := min(measure(times, timesText), measure(times, "00:00 → 00:00"))
	timesSpace := max(available-measure(label, labelText)-gap, minimumTimes)
	if timesText != "" {
		timesText = fit(times, timesText, timesSpace)
	}
	labelSpace := available
	if timesText != "" {
		labelSpace -= measure(times, timesText) + gap
	}
	baseline := centerY + capHeight(label)/2
	p.text(label, fit(label, labelText, labelSpace), labelLeft, baseline, textPrimary)
	p.text(times, timesText, right-measure(times, timesText), baseline, textPrimary)
}

func (p *painter) text(face font.Face, text string, x, baseline int, colour color.Color) {
	drawer := font.Drawer{Dst: p.canvas, Src: image.NewUniform(colour), Face: face, Dot: fixed.P(x, baseline)}
	drawer.DrawString(text)
}

func (p *painter) centered(face font.Face, text string, center, baseline int, colour color.Color) {
	p.text(face, text, center-measure(face, text)/2, baseline, colour)
}

func (p *painter) icon(icon *image.NRGBA, centerX, centerY int) {
	size := icon.Bounds().Size()
	origin := image.Pt(centerX-size.X/2, centerY-size.Y/2)
	draw.Draw(p.canvas, image.Rectangle{Min: origin, Max: origin.Add(size)}, icon, image.Point{}, draw.Over)
}

// degrees rounds to a whole degree and uses a true minus sign, which reads better than a hyphen
// at display sizes.
func degrees(value float64) string {
	rounded := int(math.Round(value))
	if rounded < 0 {
		return fmt.Sprintf("−%d°", -rounded)
	}
	return fmt.Sprintf("%d°", rounded)
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
		candidate := strings.TrimRight(string(runes[:length]), " ·,.;:-–—") + "…"
		if measure(face, candidate) <= width {
			return candidate
		}
	}
	return ""
}

// wrap breaks text into at most lines lines of width pixels, cutting the last line with an
// ellipsis when the text is longer.
func wrap(face font.Face, text string, width, lines int) []string {
	words := strings.Fields(text)
	var result []string
	for len(words) > 0 {
		if len(result) == lines-1 {
			return append(result, fit(face, strings.Join(words, " "), width))
		}
		line := words[0]
		taken := 1
		for taken < len(words) && measure(face, line+" "+words[taken]) <= width {
			line += " " + words[taken]
			taken++
		}
		result = append(result, fit(face, line, width))
		words = words[taken:]
	}
	return result
}

// wrapDetail breaks a tile's detail into at most two lines, preferring the " · " separators so a
// value stays next to its unit, such as "порывы 32".
func wrapDetail(face font.Face, text string, width int) []string {
	parts := strings.Split(text, " · ")
	for split := len(parts) - 1; split > 0; split-- {
		first, second := strings.Join(parts[:split], " · "), strings.Join(parts[split:], " · ")
		if measure(face, first) <= width && measure(face, second) <= width {
			if measure(face, text) <= width {
				return []string{text}
			}
			return []string{first, second}
		}
	}
	return wrap(face, text, width, 2)
}

type palette struct {
	top, bottom color.NRGBA
}

var (
	clearDay   = palette{color.NRGBA{58, 123, 213, 255}, color.NRGBA{31, 78, 150, 255}}
	clearNight = palette{color.NRGBA{34, 46, 82, 255}, color.NRGBA{14, 21, 44, 255}}
	cloudy     = palette{color.NRGBA{94, 110, 132, 255}, color.NRGBA{59, 71, 87, 255}}
	rainy      = palette{color.NRGBA{75, 93, 126, 255}, color.NRGBA{46, 58, 82, 255}}
	snowy      = palette{color.NRGBA{104, 126, 156, 255}, color.NRGBA{67, 84, 110, 255}}
	stormy     = palette{color.NRGBA{63, 58, 92, 255}, color.NRGBA{31, 28, 48, 255}}
)

func paletteFor(condition Condition, night bool) palette {
	var chosen palette
	switch condition {
	case Clear, PartlyCloudy:
		if night {
			return clearNight
		}
		if condition == Clear {
			return clearDay
		}
		chosen = palette{clearDay.top, cloudy.bottom}
	case Cloudy, Fog:
		chosen = cloudy
	case Drizzle, Rain, HeavyRain:
		chosen = rainy
	case Snow:
		chosen = snowy
	default:
		chosen = stormy
	}
	if night {
		chosen = palette{darken(chosen.top), darken(chosen.bottom)}
	}
	return chosen
}

func darken(c color.NRGBA) color.NRGBA {
	const factor = 0.65
	return color.NRGBA{uint8(float64(c.R) * factor), uint8(float64(c.G) * factor), uint8(float64(c.B) * factor), c.A}
}

func paintBackground(canvas *image.RGBA, colours palette) {
	height := canvas.Bounds().Dy()
	for y := 0; y < height; y++ {
		share := float64(y) / float64(height-1)
		row := color.RGBA{
			R: mix(colours.top.R, colours.bottom.R, share),
			G: mix(colours.top.G, colours.bottom.G, share),
			B: mix(colours.top.B, colours.bottom.B, share),
			A: 255,
		}
		draw.Draw(canvas, image.Rect(0, y, Width, y+1), image.NewUniform(row), image.Point{}, draw.Src)
	}
}

func mix(from, to uint8, share float64) uint8 {
	return uint8(math.Round(float64(from) + (float64(to)-float64(from))*share))
}

func fill(canvas *image.RGBA, area image.Rectangle, colour color.Color) {
	draw.Draw(canvas, area, image.NewUniform(colour), image.Point{}, draw.Over)
}

func roundedRect(canvas *image.RGBA, left, top, right, bottom, radius int, colour color.Color) {
	l, t, r, b, k := float32(left), float32(top), float32(right), float32(bottom), float32(radius)
	// 0.5523 places cubic control points so each corner approximates a quarter circle.
	c := k * (1 - 0.5523)
	size := canvas.Bounds().Size()
	path := vector.NewRasterizer(size.X, size.Y)
	path.MoveTo(l+k, t)
	path.LineTo(r-k, t)
	path.CubeTo(r-c, t, r, t+c, r, t+k)
	path.LineTo(r, b-k)
	path.CubeTo(r, b-c, r-c, b, r-k, b)
	path.LineTo(l+k, b)
	path.CubeTo(l+c, b, l, b-c, l, b-k)
	path.LineTo(l, t+k)
	path.CubeTo(l, t+c, l+c, t, l+k, t)
	path.ClosePath()
	path.DrawOp = draw.Over
	path.Draw(canvas, canvas.Bounds(), image.NewUniform(colour), image.Point{})
}
