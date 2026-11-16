// Where the compose stack answers (docs/ports.md). localhost, not
// 127.0.0.1: admin-ui's redirect URIs are exact (ADR-0058).
export const APP = "http://localhost:8080";
/** gateway-1's plain-HTTP port (dev only, docs/ports.md). */
export const GATEWAY = "http://localhost:8000";
export const ISSUER = "http://localhost:8180/auth/realms/spillway";
export const TOKEN_ENDPOINT = `${ISSUER}/protocol/openid-connect/token`;
export const AUTH_ENDPOINT = `${ISSUER}/protocol/openid-connect/auth`;
/** The dev realm's test users' password (deploy/compose/keycloak/realm.json). */
export const DEV_PASSWORD = "dev-only-not-secret";
