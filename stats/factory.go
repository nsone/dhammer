package stats

import (
	"errors"
	"dhammer/config"
)

type Stat struct {
	Name                string  `json:"stat_name"`
	Value               int     `json:"stat_value"`
	PreviousTickerValue int     `json:"stat_previous_ticker_value"`
	RatePerSecond       float64 `json:"stat_rate_per_second"`
}

type StatValue int

type Stats interface {
	AddStat(s StatValue) bool
	Init() error
	Run()
	String() string
	Stop() error
	DeInit() error
}

type StatsInitParams struct {
	options config.HammerConfig
	logFunc func(string) bool
	errFunc func(error) bool
}

var statters map[string]func(StatsInitParams) Stats = make(map[string]func(StatsInitParams) Stats)

func AddStatter(s string, f func(StatsInitParams) Stats) error {
	if _, found := statters[s]; found {
		return errors.New("Stats type already exists: " + s)
	}

	statters[s] = f

	return nil
}

func New(o config.HammerConfig, logFunc func(string) bool, errFunc func(error) bool) (Stats, error) {
	sip := StatsInitParams{
		options: o,
		logFunc: logFunc,
		errFunc: errFunc,
	}

	sf, ok := statters[o.HammerType()]

	if !ok {
		return nil, errors.New("Statters - Hammer type not found: " + o.HammerType())
	}

	return sf(sip), nil
}
