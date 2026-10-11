package state

import "strings"

// SiteJobs lists a site's jobs of the given kinds, oldest first, without
// their logs. The API reads the payloads of later settings changes from it
// to tell which fields a later request set (SPEC.md "Sites").
func (s *Store) SiteJobs(siteID int64, kinds ...string) ([]Job, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	args := []any{siteID}
	for _, k := range kinds {
		args = append(args, k)
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(kinds)), ", ")
	return s.queryJobs(`SELECT `+jobColsNoLog+` FROM jobs WHERE site_id = ? AND kind IN (`+marks+`) ORDER BY id`, args...)
}
