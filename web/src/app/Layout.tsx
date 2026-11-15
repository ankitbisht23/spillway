import { Link, Outlet } from "@tanstack/react-router";

import { can } from "../auth/permissions";
import { useSession } from "../auth/SessionContext";
import { navigation, screenOp } from "./screens";

/** The app shell: header with the user, the navigation the user's roles allow, and the current screen. */
export function Layout() {
  const { user, signOut } = useSession();
  const shown = navigation.filter((s) => {
    const op = screenOp(s);
    return op === undefined || can(user.roles, op);
  });
  return (
    <div className="shell">
      <header className="topbar">
        <Link to="/" className="brand">Spillway</Link>
        <div className="user">
          <span>{user.name}</span>
          <span className="roles" aria-label="Roles">{user.roles.join(", ")}</span>
          <button type="button" onClick={signOut}>Sign out</button>
        </div>
      </header>
      <nav className="sidebar" aria-label="Screens">
        <ul>
          {shown.map((s) => (
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
