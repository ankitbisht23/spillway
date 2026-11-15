// The one mock of the network (D10, ADR-0061): MSW at the fetch boundary.
// An unmocked request fails its test.
import { setupServer } from "msw/node";

export const server = setupServer();
