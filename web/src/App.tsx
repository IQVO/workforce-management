import { Navigate, NavLink, Outlet, Route, Routes } from "react-router-dom";
import { WorkforceScreen } from "./screens/WorkforceScreen";

const SUB_NAV = [{ to: "staffing-gaps", label: "Staffing gaps", end: true }];

const linkStyle = ({ isActive }: { isActive: boolean }) => ({
  display: "inline-flex",
  padding: "6px 12px",
  borderRadius: "var(--wh-radius-pill)",
  fontSize: "var(--wh-font-size-sm)",
  fontWeight: isActive ? 600 : 500,
  color: isActive ? "var(--wh-color-text)" : "var(--wh-color-text-muted)",
  background: isActive ? "var(--wh-color-accent-muted)" : "transparent",
  textDecoration: "none",
});

/** The sub-nav, rendered by a layout route with the path "/".
 *
 *  The console mounts this component inside its own `<Route
 *  path="/workforce/*">`. A relative `<NavLink>` rendered straight in
 *  that splat route resolves against the FULL current URL (react-router 7
 *  uses the last path-contributing match's pathname, splat included), so
 *  from /workforce/associates the link `staffing-gaps` would point at
 *  /workforce/associates/staffing-gaps. A pathless layout route does not
 *  help (react-router drops pathless routes from relative resolution); a
 *  layout route WITH path "/" does: its own match is the mount point,
 *  under any prefix and standalone at `/`. (Same fix as the
 *  inbound-receiving and product-master remotes.) */
function WorkforceLayout() {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "var(--wh-space-5)" }}>
      <nav aria-label="Workforce sections" style={{ display: "flex", gap: "var(--wh-space-2)" }}>
        {SUB_NAV.map((item) => (
          <NavLink key={item.to} to={item.to} end={item.end} style={linkStyle}>
            {item.label}
          </NavLink>
        ))}
      </nav>
      <Outlet />
    </div>
  );
}

/** Exposed as workforce_mfe/App via Module Federation. Takes NO props: the
 *  console mounts it under /workforce/* and provides the BrowserRouter,
 *  the design tokens and `window.__WAREHOUSE_CONFIG__`. Routes here are
 *  RELATIVE, so the component works identically under that prefix (in the
 *  shell) or at / (standalone dev, see main.tsx).
 *
 *  - staffing-gaps: planned vs present headcount per path -- by site
 *    (canonical, ADR 0035), by building (deprecated key, kept), or one
 *    path at a time. */
export default function App() {
  return (
    <Routes>
      <Route path="/" element={<WorkforceLayout />}>
        <Route index element={<Navigate to="staffing-gaps" replace />} />
        <Route path="staffing-gaps" element={<WorkforceScreen />} />
      </Route>
    </Routes>
  );
}
