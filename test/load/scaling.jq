# NFR-02's probe rule (ADR-0074 §D9, layout and limit changed by ADR-0084),
# included by scaling.sh and tested by scaling_test.go. Input: one probe's
# result with its vCPU readings (.vcpu.gateway, .loadgen, .upstream).

# A probe of the gateway on n vCPUs is gateway-bound when the gateway used
# at least min_gw × n vCPU and loadgen and the upstream, which share CPUs 1
# and 3, together at most max_side vCPU.
def gateway_bound($n; $min_gw; $max_side):
	.vcpu.gateway >= $min_gw * $n and .vcpu.loadgen + .vcpu.upstream <= $max_side;

# Why a probe is not gateway-bound, for invalid_reasons.
def not_bound_reason($n; $i; $min_gw; $max_side):
	"n=\($n) probe \($i): not gateway-bound (vCPU gateway \(.vcpu.gateway * 100 | round / 100) < \($min_gw * $n) or loadgen + upstream \((.vcpu.loadgen + .vcpu.upstream) * 100 | round / 100) > \($max_side * 100 | round / 100))";
