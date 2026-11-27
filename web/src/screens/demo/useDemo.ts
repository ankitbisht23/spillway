// Demo mode (SC-08, ADR-0095 §2): GET /api/v1/demo answers 404 unless the
// control plane runs in demo mode. The UI asks once per session; 404 is
// not transient, so it is never retried.
import { useQuery } from "@tanstack/react-query";

import { api, call } from "../../api/client";

/** The demo, or an error: 404 when demo mode is off. */
export function useDemo() {
  return useQuery({ queryKey: ["demo"], queryFn: async () => (await call(api.GET("/demo"))).data, staleTime: Infinity });
}
