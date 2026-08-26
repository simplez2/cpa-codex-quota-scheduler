package main

import (
	"math"

	"gopkg.in/yaml.v3"
)

// UnmarshalYAML rejects YAML floating-point specials before parsePluginConfig
// applies ordinary range validation. Comparisons such as NaN <= 0 and NaN >
// 100 are both false, so allowing .nan through the default decoder would make
// an otherwise bounded percentage silently become non-finite runtime state.
func (in *yamlPluginConfig) UnmarshalYAML(node *yaml.Node) error {
	type plainPluginConfig yamlPluginConfig
	var decoded plainPluginConfig
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*in = yamlPluginConfig(decoded)

	sanitizeFinite := func(value **float64) {
		if value == nil || *value == nil {
			return
		}
		if math.IsNaN(**value) || math.IsInf(**value, 0) {
			*value = nil
		}
	}

	sanitizeFinite(&in.SerialSwitchPercent)
	sanitizeFinite(&in.Serial5hSwitchPercent)
	sanitizeFinite(&in.DrainWindowHours)
	sanitizeFinite(&in.SoftLimitPercent)
	sanitizeFinite(&in.Reserve5hPercent)
	sanitizeFinite(&in.ReserveWeeklyPercent)
	sanitizeFinite(&in.ReserveMonthlyPercent)
	sanitizeFinite(&in.LowQuotaPercent)
	sanitizeFinite(&in.SwitchHysteresisPercent)
	sanitizeFinite(&in.NormalCostQuantile)
	sanitizeFinite(&in.GuardCostQuantile)
	sanitizeFinite(&in.HighCostQuantile)
	return nil
}
