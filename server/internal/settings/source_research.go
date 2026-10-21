package settings

// SourceResearch limits one collection round, independently of model resources.
type SourceResearch struct {
	MaxSourcesPerRound int `json:"max_sources_per_round"`
	MaxPagesPerRound   int `json:"max_pages_per_round"`
}

func DefaultSourceResearch() SourceResearch { return SourceResearch{10, 30} }
func (s SourceResearch) Validate() error {
	if s.MaxSourcesPerRound < 1 || s.MaxSourcesPerRound > 20 || s.MaxPagesPerRound < 1 || s.MaxPagesPerRound > 100 || s.MaxSourcesPerRound > s.MaxPagesPerRound {
		return ErrInvalid
	}
	return nil
}
