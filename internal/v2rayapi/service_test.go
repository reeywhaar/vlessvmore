package v2rayapi

import (
	"strings"
	"testing"
)

// The service name is the one thing about this vendored client that protoc gets wrong for
// our purposes: sing-box renames the service at init time for v2ray compatibility, and
// that rename is in a hand-written file we do not vendor.
//
// Getting it wrong fails at runtime, not at build time, and only once a real sing-box is
// on the other end — so it is worth pinning here rather than discovering in production:
//
//	rpc error: code = Unimplemented desc = unknown service experimental.v2rayapi.StatsService
func TestServiceNameMatchesSingBox(t *testing.T) {
	const want = "v2ray.core.app.stats.command.StatsService"
	if ServiceName != want {
		t.Errorf("ServiceName = %q, want %q — sing-box will answer Unimplemented", ServiceName, want)
	}
	// The proto package name is what protoc generates and what a re-vendor would
	// reintroduce.
	if strings.Contains(ServiceName, "experimental.v2rayapi") {
		t.Error("ServiceName is protoc's generated name; sing-box does not register that")
	}
}

func TestFullMethodNamesUseTheRegisteredService(t *testing.T) {
	tests := map[string]string{
		"GetStats":    StatsService_GetStats_FullMethodName,
		"QueryStats":  StatsService_QueryStats_FullMethodName,
		"GetSysStats": StatsService_GetSysStats_FullMethodName,
	}
	for method, full := range tests {
		want := "/" + ServiceName + "/" + method
		if full != want {
			t.Errorf("%s full method = %q, want %q", method, full, want)
		}
	}
}
