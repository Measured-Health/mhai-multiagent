package disclose

import "regexp"

var opaqueID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// CheckFilters is D4: more than FilterCap filters is ErrTooManyFilters
// (422 too_many_filters); a value that isn't one of the contract's codes,
// or not an opaque id, is ErrInvalid. Whether an id is the hospital's is
// the database's.
func CheckFilters(f Filters) error {
	if f.Count() > FilterCap {
		return ErrTooManyFilters
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
