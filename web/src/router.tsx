import { QueryClient } from "@tanstack/react-query";
import { createRootRoute, createRoute, createRouter } from "@tanstack/react-router";
import { ApiError } from "./api";
import { Shell } from "./components/Shell";
import { SetupPage } from "./pages/Setup";
import { SignInPage } from "./pages/SignIn";
import { PlatformWizard } from "./pages/wizard/PlatformWizard";
import { WorkspaceWizard } from "./pages/workspace/WorkspaceWizard";
import { FleetPage } from "./pages/Fleet";
import { RoomPage, roomTabs, type RoomTab } from "./pages/room/RoomPage";
import { ApprovalsPage } from "./pages/Approvals";
import { InboxPage } from "./pages/Inbox";
import { AuditPage } from "./pages/Audit";
import { SpendPage } from "./pages/Spend";
import { UsersPage } from "./pages/Users";
import { NotificationsPage } from "./pages/Notifications";
import { WorkspacesPage } from "./pages/Workspaces";
import { ChannelsPage, ChannelView, MessageView, type FeedScope } from "./pages/messages/ChannelsPage";
import { HomeRedirect, ObservatoryPage } from "./pages/observatory/ObservatoryPage";

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: (n, e) => !(e instanceof ApiError && ["unauthenticated", "permission_denied", "not_found"].includes(e.code)) && n < 2,
      refetchOnWindowFocus: false,
    },
  },
});

const rootRoute = createRootRoute({ component: Shell });

const r = <P extends string>(path: P, component: () => React.ReactNode) =>
  createRoute({ getParentRoute: () => rootRoute, path, component });

const indexRoute = createRoute({ getParentRoute: () => rootRoute, path: "/", component: HomeRedirect });

const observatoryRoute = createRoute({ getParentRoute: () => rootRoute, path: "/w/$workspace", component: ObservatoryPage });

const signinRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/signin",
  validateSearch: (s: Record<string, unknown>): { next?: string; error?: string } => ({
    next: typeof s.next === "string" ? s.next : undefined,
    error: typeof s.error === "string" ? s.error : undefined,
  }),
  component: SignInPage,
});

const roomRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/rooms/$workspace/$room",
  validateSearch: (s: Record<string, unknown>): { tab: RoomTab } => ({
    tab: roomTabs.includes(s.tab as RoomTab) ? (s.tab as RoomTab) : "agent",
  }),
  component: RoomPage,
});

const newWorkspaceRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/workspaces/new",
  component: () => <WorkspaceWizard />,
});

const editWorkspaceRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/workspaces/$workspace/edit",
  component: function Edit() {
    const { workspace } = editWorkspaceRoute.useParams();
    return <WorkspaceWizard existing={workspace} />;
  },
});

const approvalRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/approvals/$id",
  component: function One() {
    const { id } = approvalRoute.useParams();
    return <ApprovalsPage focus={id} />;
  },
});

const conversationRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/inbox/$address",
  component: function Conv() {
    const { address } = conversationRoute.useParams();
    return <InboxPage address={address} />;
  },
});

const channelsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/channels",
  validateSearch: (s: Record<string, unknown>): { scope: FeedScope } => ({
    scope: s.scope === "mentions" || s.scope === "all" ? s.scope : "unread",
  }),
  component: ChannelsPage,
});

const channelRoute = createRoute({ getParentRoute: () => rootRoute, path: "/channels/$workspace/$channel", component: ChannelView });
const messageRoute = createRoute({ getParentRoute: () => rootRoute, path: "/channels/$workspace/message/$id", component: MessageView });

const routeTree = rootRoute.addChildren([
  indexRoute,
  observatoryRoute,
  r("/setup", SetupPage),
  signinRoute,
  r("/wizard", PlatformWizard),
  newWorkspaceRoute,
  editWorkspaceRoute,
  r("/workspaces", WorkspacesPage),
  r("/fleet", FleetPage),
  roomRoute,
  r("/approvals", () => <ApprovalsPage />),
  approvalRoute,
  r("/inbox", () => <InboxPage />),
  channelsRoute,
  channelRoute,
  messageRoute,
  conversationRoute,
  r("/audit", AuditPage),
  r("/spend", SpendPage),
  r("/users", UsersPage),
  r("/settings/notifications", NotificationsPage),
]);

export const router = createRouter({ routeTree, defaultPreload: false });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

