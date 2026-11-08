package disclose

import "regexp"

var opaqueID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// CheckFilters is D4: more than FilterCap filters is ErrTooManyFilters
// (422 too_many_filters); a value that isn't one of the contract's codes,
// or not an opaque id, is ErrInvalid. Whether an id is the hospital's is
// the database's.
//
// A location and a setting together are ErrInvalid too: a location has its
// setting, so the pair is the location alone or nothing, and its two
// parents can split the unfiltered view between them (P2 found a hidden
// cell solvable within that family). Refused, the two parents share no
// family; what is left between them is R-A1, which P2 measures.
func CheckFilters(f Filters) error {
	if f.Count() > FilterCap {
		return ErrTooManyFilters
	}
	if f[DimLocation] != "" && f[DimSetting] != "" {
		return ErrInvalid
	}
	codes := map[int][]string{DimAgeBand: AgeBands, DimSex: Sexes, DimSetting: Settings}
	for d, v := range f {
		if v == "" {
			continue
		}
		if list, ok := codes[d]; ok {
			if !contains(list, v) {
				return ErrInvalid
			}
			continue
		}
		if !opaqueID.MatchString(v) {
			return ErrInvalid
		}
	}
	return nil
}
