package discovery

import "regexp"

var userFriendlyAddrTemplate = regexp.MustCompile(`^[EUk0][Qq][A-Za-z0-9_-]{46}$`)

func IsUserFriendly(s string) bool {
	return len(s) == 48 && userFriendlyAddrTemplate.MatchString(s)
}
