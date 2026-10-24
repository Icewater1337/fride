package review

import (
	"path"
	"regexp"
	"strings"
)

// Layers, in the order a change reads best: what the data looks like, the
// logic over it, the interface that exposes it, what uses that interface, and
// the tests that pin it all down. Docs and build files close the tour.
var Layers = []string{"schema", "logic", "api", "consumer", "test", "other"}

var (
	schemaPath = regexp.MustCompile(`(?i)((^|/)(migrations?|migrate|schema|schemas|db/changelog|liquibase|flyway|models?|entities|entity|domain)(/|$)|\.(sql|proto|graphqls?|gql|prisma|avsc)$|(^|/)schema\.[a-z]+$)`)
	apiPath    = regexp.MustCompile(`(?i)((^|/)(api|apis|handlers?|controllers?|routes?|routers?|endpoints?|resolvers?|server|rpc|grpc|rest)(/|$)|(^|[/_.-])(handler|handlers|controller|controllers|routes|router|server|api|endpoint|endpoints|resolver|resolvers)\.[a-z]+$)`)
	usePath    = regexp.MustCompile(`(?i)((^|/)(web|ui|frontend|client|clients|app|pages|views|components|screens|cmd|cli)(/|$)|\.(tsx|jsx|vue|svelte)$|(^|/)main\.[a-z]+$)`)
	styleFile  = regexp.MustCompile(`(?i)\.(css|scss|less|html)$`)
)

// layerOf places a file in the tour by its path. It is a guess from naming,
// as the noise rules are: the order only suggests where to start reading.
//
// ponytail: path heuristics, not an import graph. Upgrade by reading each
// language's imports if the order misleads.
func layerOf(p string) string {
	switch {
	case IsTest(p):
		return "test"
	case schemaPath.MatchString(p):
		return "schema"
	case styleFile.MatchString(p):
		return "consumer"
	case !isCode(p) || manifests[path.Base(p)] || strings.HasPrefix(p, "docs/"):
		return "other"
	case apiPath.MatchString(p):
		return "api"
	case usePath.MatchString(p):
		return "consumer"
	}
	return "logic"
}

func layerRank(l string) int {
	for i, x := range Layers {
		if x == l {
			return i
		}
	}
	return len(Layers)
}
