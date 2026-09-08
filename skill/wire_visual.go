package skill

import "fmt"

type SkillPresentation struct {
	IconKeywords []string   `json:"icon_keywords"`
	Cast         *VisualRef `json:"cast"`
}

type VisualRef struct {
	Category string   `json:"category"`
	Theme    string   `json:"theme"`
	Elements []string `json:"elements"`
}

func (value *SkillPresentation) UnmarshalJSON(data []byte) error {
	var raw struct {
		IconKeywords []string   `json:"icon_keywords"`
		Cast         *VisualRef `json:"cast"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return err
	}
	*value = SkillPresentation{IconKeywords: append([]string(nil), raw.IconKeywords...), Cast: raw.Cast}
	return nil
}

func (value *VisualRef) UnmarshalJSON(data []byte) error {
	var raw struct {
		Category string   `json:"category"`
		Theme    string   `json:"theme"`
		Elements []string `json:"elements"`
	}
	if err := decodeStrictSingle(data, &raw); err != nil {
		return fmt.Errorf("visual: %w", err)
	}
	*value = VisualRef{Category: raw.Category, Theme: raw.Theme, Elements: append([]string(nil), raw.Elements...)}
	return nil
}
