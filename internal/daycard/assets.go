package daycard

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
	"sync"

	scale "golang.org/x/image/draw"
	"golang.org/x/image/font/opentype"
)

//go:embed assets/fonts/*.ttf assets/icons/large/*.png assets/icons/small/*.png
var assetFiles embed.FS

type iconVariants struct {
	day, night string
}

var iconFiles = [...]iconVariants{
	Clear:        {"clear-day", "clear-night"},
	PartlyCloudy: {"partly-cloudy-day", "partly-cloudy-night"},
	Cloudy:       {"cloudy", "cloudy"},
	Fog:          {"fog-day", "fog-night"},
	Drizzle:      {"drizzle", "drizzle"},
	Rain:         {"rain", "rain"},
	HeavyRain:    {"rain", "rain"},
	Snow:         {"snow", "snow"},
	Thunderstorm: {"thunderstorms-rain", "thunderstorms-rain"},
}

type assetSet struct {
	regular, semibold *opentype.Font
	large, small      map[string]*image.NRGBA
}

var loadAssets = sync.OnceValues(func() (*assetSet, error) {
	set := &assetSet{large: map[string]*image.NRGBA{}, small: map[string]*image.NRGBA{}}
	var err error
	if set.regular, err = loadFont("assets/fonts/Inter-Regular.ttf"); err != nil {
		return nil, err
	}
	if set.semibold, err = loadFont("assets/fonts/Inter-SemiBold.ttf"); err != nil {
		return nil, err
	}
	for _, variants := range iconFiles {
		for _, name := range []string{variants.day, variants.night} {
			if set.large[name], err = loadIcon("assets/icons/large/"+name+".png", largeIconLimit); err != nil {
				return nil, err
			}
			if set.small[name], err = loadIcon("assets/icons/small/"+name+".png", smallIconLimit); err != nil {
				return nil, err
			}
		}
	}
	return set, nil
})

func (set *assetSet) icon(size map[string]*image.NRGBA, condition Condition, night bool) *image.NRGBA {
	variants := iconFiles[condition]
	if night {
		return size[variants.night]
	}
	return size[variants.day]
}

func loadFont(name string) (*opentype.Font, error) {
	data, err := assetFiles.ReadFile(name)
	if err != nil {
		return nil, err
	}
	parsed, err := opentype.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

// Icons are cropped to their visible pixels and shrunk to fit these boxes. Meteocons draw inside a
// padded 64×64 view box whose padding differs per icon, and the sun is much taller than a cloud,
// so layout works on the visible shape and caps its height.
var (
	largeIconLimit = image.Pt(200, 180)
	smallIconLimit = image.Pt(100, 84)
)

func loadIcon(name string, limit image.Point) (*image.NRGBA, error) {
	data, err := assetFiles.ReadFile(name)
	if err != nil {
		return nil, err
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	full := image.NewNRGBA(decoded.Bounds().Sub(decoded.Bounds().Min))
	draw.Draw(full, full.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	visible := image.Rectangle{}
	for y := 0; y < full.Rect.Dy(); y++ {
		for x := 0; x < full.Rect.Dx(); x++ {
			if full.Pix[full.PixOffset(x, y)+3] > 8 {
				visible = visible.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	if visible.Empty() {
		return nil, fmt.Errorf("icon %s is empty", name)
	}
	ratio := min(1, float64(limit.X)/float64(visible.Dx()), float64(limit.Y)/float64(visible.Dy()))
	if ratio == 1 {
		cropped := image.NewNRGBA(image.Rect(0, 0, visible.Dx(), visible.Dy()))
		draw.Draw(cropped, cropped.Bounds(), full, visible.Min, draw.Src)
		return cropped, nil
	}
	width := int(math.Round(float64(visible.Dx()) * ratio))
	height := int(math.Round(float64(visible.Dy()) * ratio))
	scaled := image.NewNRGBA(image.Rect(0, 0, width, height))
	scale.CatmullRom.Scale(scaled, scaled.Bounds(), full, visible, draw.Src, nil)
	return scaled, nil
}
