import { Link, Outlet } from "@tanstack/react-router";

import { navigation } from "./screens";

/** The app shell: header, navigation and the current screen. */
export function Layout() {
  return (
    <div className="shell">
      <header className="topbar">
        <Link to="/" className="brand">Spillway</Link>
      </header>
      <nav className="sidebar" aria-label="Screens">
        <ul>
          {navigation.map((s) => (
            <li key={s.to}>
              <Link to={s.to} activeProps={{ "aria-current": "page", className: "active" }}>{s.label}</Link>
            </li>
          ))}
        </ul>
      </nav>
      <main className="content">
        <Outlet />
      </main>
    </div>
  );
}
