import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, Navigate, Outlet, useLocation, useNavigate } from "@tanstack/react-router";
import { api, ApiError } from "@/api";
import { useApprovals, useApprovalsFeed } from "@/lib/live";
import { useTheme, type Theme } from "@/lib/theme";
import { Spinner, cx } from "./ui";

const publicPaths = ["/setup", "/signin"];
const wizardSteps = ["preflight", "signin", "services", "hostname"];

export function useMe() {
  return useQuery({ queryKey: ["me"], queryFn: () => api().me(), retry: false, staleTime: 60_000 });
}

/** Decides where a person may be (setup, sign-in, wizard), then renders the page. */
export function Shell() {
  const { pathname } = useLocation();
  const setup = useQuery({ queryKey: ["setup"], queryFn: () => api().getSetupState() });
  const me = useMe();
  const isAdmin = me.data?.role === "admin";
  const platform = useQuery({ queryKey: ["platform"], queryFn: () => api().getPlatform(), enabled: isAdmin });
  useApprovalsFeed(!!me.data);

  const isPublic = publicPaths.includes(pathname);
  if (setup.isPending || (me.isPending && !isPublic)) return <FullPage><Spinner label="Loading" /></FullPage>;
  if (setup.data && !setup.data.adminClaimed && !me.data && pathname !== "/setup") return <Navigate to="/setup" />;
  if (!me.data && !isPublic) {
    const unauth = !me.error || (me.error instanceof ApiError && me.error.code === "unauthenticated");
    if (unauth) return <Navigate to="/signin" search={{ next: pathname }} />;
    return <FullPage><p role="alert" className="text-bad">{String(me.error)}</p></FullPage>;
  }
  if (me.data && isPublic && setup.data?.adminClaimed) return <Navigate to="/" />;
  if (isAdmin && platform.isPending) return <FullPage><Spinner label="Loading" /></FullPage>;

  const step = platform.data?.spec.wizardStep;
  if (step && wizardSteps.includes(step) && pathname !== "/wizard") return <Navigate to="/wizard" />;
  if (step === "workspace" && !["/wizard", "/workspaces/new"].includes(pathname)) return <Navigate to="/workspaces/new" />;

  const bare = isPublic || pathname === "/wizard" || (step === "workspace" && pathname === "/workspaces/new");
  // One layout for both, so switching (the wizard finishing) doesn't remount the page.
  return (
    <div className="flex min-h-full flex-col">
      <a href="#main" className="sr-only focus:not-sr-only focus:absolute focus:z-50 focus:bg-surface focus:p-2">
        Skip to content
      </a>
      <header className="sticky top-0 z-30 border-b border-border bg-surface">{bare ? <BareHeader /> : <AppHeader />}</header>
      <main id="main" className={cx("mx-auto w-full flex-1 p-4", bare ? "max-w-4xl" : "max-w-7xl")}>
        <Outlet />
      </main>
    </div>
  );
}

function FullPage({ children }: { children: React.ReactNode }) {
  return <div className="flex min-h-full items-center justify-center p-4">{children}</div>;
}

function Brand() {
  return (
    <span className="flex items-center gap-2 font-semibold">
      <img src="/icon.svg" alt="" className="size-6" />
      swarmageddon
    </span>
  );
}

function BareHeader() {
  return (
    <div className="flex items-center justify-between px-4 py-2">
      <Brand />
      <ThemeSwitch />
    </div>
  );
}

function AppHeader() {
  const me = useMe();
  const approvals = useApprovals();
  const pending = approvals.data?.length ?? 0;
  const qc = useQueryClient();
  const navigate = useNavigate();
  const logout = useMutation({
    mutationFn: () => api().logout(),
    onSuccess: () => {
      qc.clear();
      void navigate({ to: "/signin" });
    },
  });
  const workspaces = useQuery({ queryKey: ["workspaces"], queryFn: () => api().listWorkspaces() });
  const firstWs = workspaces.data?.[0]?.name;
  const links: { to: string; label: string; badge?: number; admin?: boolean }[] = [
    ...(firstWs ? [{ to: `/w/${firstWs}`, label: "Observatory" }] : []),
    { to: "/fleet", label: "Fleet" },
    { to: "/approvals", label: "Approvals", badge: pending },
    { to: "/channels", label: "Messages" },
    { to: "/workspaces", label: "Workspaces" },
    { to: "/spend", label: "Spend" },
    { to: "/audit", label: "Audit" },
    { to: "/users", label: "Users", admin: true },
    { to: "/settings/notifications", label: "Notifications" },
  ];
  return (
    <>
      <div className="flex items-center justify-between gap-2 px-4 py-2">
        <Link to="/" aria-label="swarmageddon home">
          <Brand />
        </Link>
        <div className="flex items-center gap-2">
          <ThemeSwitch />
          <span className="hidden text-sm text-muted sm:inline" data-testid="whoami">
            {me.data?.displayName} ({me.data?.role})
          </span>
          <button type="button" onClick={() => logout.mutate()} className="min-h-10 rounded-xs px-2 text-sm hover:bg-surface-2">
            Sign out
          </button>
        </div>
      </div>
      <nav aria-label="Main" className="overflow-x-auto">
        <ul className="flex gap-1 px-2 pb-1">
          {links
            .filter((l) => !l.admin || me.data?.role === "admin")
            .map((l) => (
              <li key={l.to}>
                <Link
                  to={l.to}
                  className="flex min-h-10 items-center gap-1 rounded-xs px-3 text-sm whitespace-nowrap text-muted hover:bg-surface-2 hover:text-fg"
                  activeProps={{ className: "bg-surface-2 !text-fg font-medium", "aria-current": "page" }}
                >
                  {l.label}
                  {!!l.badge && (
                    <span className="rounded-full bg-bad px-1.5 text-xs font-semibold text-white" data-testid="approvals-badge">
                      {l.badge}
                      <span className="sr-only"> pending</span>
                    </span>
                  )}
                </Link>
              </li>
            ))}
        </ul>
      </nav>
    </>
  );
}

function ThemeSwitch() {
  const [theme, setTheme] = useTheme();
  const order: Theme[] = ["system", "light", "dark"];
  const next = order[(order.indexOf(theme) + 1) % order.length];
  return (
    <button
      type="button"
      onClick={() => setTheme(next)}
      className="min-h-10 min-w-10 rounded-xs px-2 text-sm hover:bg-surface-2"
      aria-label={`Theme: ${theme}. Switch to ${next}`}
      title={`Theme: ${theme}`}
    >
      <svg viewBox="0 0 20 20" className="size-5" aria-hidden>
        <circle cx="10" cy="10" r="7" fill={theme === "dark" ? "currentColor" : "none"} stroke="currentColor" strokeWidth="2" />
        {theme === "system" && <path d="M10 3 A7 7 0 0 1 10 17 Z" fill="currentColor" />}
      </svg>
    </button>
  );
}
