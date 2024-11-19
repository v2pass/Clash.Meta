package constant

import (
	"github.com/Ruk1ng001/Clash.Meta/component/geodata/router"
)

type RuleGeoSite interface {
	GetDomainMatcher() *router.DomainMatcher
}

type RuleGeoIP interface {
	GetIPMatcher() *router.GeoIPMatcher
}

type RuleGroup interface {
	GetRecodeSize() int
}
