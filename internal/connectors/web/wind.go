package web

import (
	"fmt"
	"math"
)

// windUnit is how a forecast prints wind speeds; Open-Meteo always answers in km/h and the tools
// convert, so the model never converts by hand.
type windUnit string

const (
	windKilometres windUnit = "kmh"
	windMetres     windUnit = "ms"
	windKnots      windUnit = "kn"
	windBeaufort   windUnit = "bft"
)

// windSymbols are the English unit symbols; a card in another language has its own.
var windSymbols = map[windUnit]string{windKilometres: "km/h", windMetres: "m/s", windKnots: "kn", windBeaufort: "Bft"}

// beaufortLimits are the upper bounds, in m/s, of Beaufort forces 0 to 11; anything above is 12.
var beaufortLimits = []float64{0.5, 1.6, 3.4, 5.5, 8.0, 10.8, 13.9, 17.2, 20.8, 24.5, 28.5, 32.7}

func parseWindUnit(value string) (windUnit, error) {
	switch unit := windUnit(value); unit {
	case "":
		return windKilometres, nil
	case windKilometres, windMetres, windKnots, windBeaufort:
		return unit, nil
	default:
		return "", fmt.Errorf(`wind_unit must be "kmh", "ms", "kn", or "bft"`)
	}
}

// format prints a speed given in km/h in the unit, with its English symbol.
func (u windUnit) format(kmh float64) string {
	return u.formatWith(kmh, windSymbols)
}

func (u windUnit) formatWith(kmh float64, symbols map[windUnit]string) string {
	symbol := symbols[u]
	metres := kmh / 3.6
	switch u {
	case windMetres:
		return fmt.Sprintf("%.0f %s", metres, symbol)
	case windKnots:
		return fmt.Sprintf("%.0f %s", kmh/1.852, symbol)
	case windBeaufort:
		rounded := math.Round(metres*10) / 10
		for force, limit := range beaufortLimits {
			if rounded < limit {
				return fmt.Sprintf("%s %d", symbol, force)
			}
		}
		return symbol + " 12"
	default:
		return fmt.Sprintf("%.0f %s", kmh, symbol)
	}
}
