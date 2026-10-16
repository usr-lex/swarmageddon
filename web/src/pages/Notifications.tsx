import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { api } from "@/api";
import { notificationEvents, type ChannelKind, type NotificationEvent, type NotificationPrefs } from "@/api/types";
import { useMe } from "@/components/Shell";
import { Badge, Button, Card, ErrorText, PageHeader, Spinner } from "@/components/ui";
import { currentSubscription, disablePush, enablePush, pushSupported } from "@/lib/push";
import { channelLabels, eventLabels } from "./workspace/steps";

const allChannels: ChannelKind[] = ["webPush", "ntfy", "slack", "discord", "email"];

/** Browser push and each person's channels per event and workspace (§10.3). */
export function NotificationsPage() {
  const me = useMe();
  const qc = useQueryClient();
  const prefs = useQuery({ queryKey: ["prefs"], queryFn: () => api().getNotificationPrefs() });
  const [draft, setDraft] = useState<NotificationPrefs>();
  useEffect(() => {
    if (prefs.data && !draft) setDraft(prefs.data);
  }, [prefs.data, draft]);
  const save = useMutation({ mutationFn: (p: NotificationPrefs) => api().setNotificationPrefs(p), onSuccess: () => qc.invalidateQueries({ queryKey: ["prefs"] }) });
  // Admins get an empty workspace list from GetMe (they see all).
  const all = useQuery({ queryKey: ["workspaces"], queryFn: () => api().listWorkspaces(), enabled: me.data?.role === "admin" });
  const workspaces = me.data?.role === "admin" ? (all.data?.map((w) => w.name) ?? []) : (me.data?.workspaces ?? []);
  const channels = allChannels.filter((c) => c === "webPush" || draft?.availableChannels.includes(c));
  const test = useMutation({ mutationFn: (c: ChannelKind) => api().sendTestNotification(c) });

  const find = (ws: string, ev: NotificationEvent) =>
    draft?.prefs.find((p) => p.workspace === ws && p.event === ev) ?? draft?.prefs.find((p) => p.workspace === "" && p.event === ev);
  const isOn = (ws: string, ev: NotificationEvent, ch: ChannelKind) => {
    const p = find(ws, ev);
    // Default: browser push for everything until the person chooses.
    return p ? p.channels.includes(ch) : ch === "webPush";
  };
  const toggle = (ws: string, ev: NotificationEvent, ch: ChannelKind, on: boolean) => {
    if (!draft) return;
    const cur = channels.filter((c) => isOn(ws, ev, c));
    const next = on ? [...cur, ch] : cur.filter((c) => c !== ch);
    const rest = draft.prefs.filter((p) => !(p.workspace === ws && p.event === ev));
    setDraft({ ...draft, prefs: [...rest, { workspace: ws, event: ev, channels: next }] });
  };

  return (
    <div className="space-y-4">
      <PageHeader title="Notifications" />
      <PushCard />
      {prefs.isPending && <Spinner label="Loading" />}
      <ErrorText error={prefs.error} />
      {draft &&
        workspaces.map((ws) => (
          <Card key={ws} title={`Workspace ${ws}`}>
            <div className="overflow-x-auto">
              <table className="w-full text-left text-sm">
                <caption className="sr-only">Channels per event in {ws}</caption>
                <thead className="text-xs text-muted">
                  <tr>
                    <th scope="col" className="py-1 pr-2 font-medium">
                      Event
                    </th>
                    {channels.map((c) => (
                      <th key={c} scope="col" className="px-2 py-1 text-center font-medium">
                        {channelLabels[c]}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {notificationEvents.map((ev) => (
                    <tr key={ev}>
                      <th scope="row" className="py-2 pr-2 font-normal">
                        {eventLabels[ev]}
                      </th>
                      {channels.map((c) => (
                        <td key={c} className="px-2 py-2 text-center">
                          <input
                            type="checkbox"
                            className="size-5 accent-accent"
                            aria-label={`${eventLabels[ev]} via ${channelLabels[c]} in ${ws}`}
                            checked={isOn(ws, ev, c)}
                            onChange={(e) => toggle(ws, ev, c, e.target.checked)}
                          />
                        </td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Card>
        ))}
      {draft && workspaces.length === 0 && <p className="text-sm text-muted">You're not assigned to any workspace yet.</p>}
      {draft && (
        <Card title="Test a channel">
          <div className="flex flex-wrap gap-2">
            {channels.map((c) => (
              <Button key={c} onClick={() => test.mutate(c)} busy={test.isPending && test.variables === c}>
                Send a test via {channelLabels[c]}
              </Button>
            ))}
          </div>
          {test.isSuccess && <p role="status" className="mt-2 text-sm text-ok">Sent.</p>}
          <ErrorText error={test.error} />
        </Card>
      )}
      {draft && workspaces.length > 0 && (
        <div className="flex items-center justify-end gap-2">
          {save.isSuccess && <span role="status" className="text-sm text-ok">Saved.</span>}
          <ErrorText error={save.error} />
          <Button variant="primary" busy={save.isPending} onClick={() => save.mutate(draft)}>
            Save preferences
          </Button>
        </div>
      )}
    </div>
  );
}

function PushCard() {
  const [subscribed, setSubscribed] = useState<boolean>();
  useEffect(() => {
    void currentSubscription().then((s) => setSubscribed(!!s), () => setSubscribed(false));
  }, []);
  const toggle = useMutation({
    mutationFn: async () => {
      if (subscribed) await disablePush();
      else await enablePush();
    },
    onSuccess: () => setSubscribed(!subscribed),
  });
  return (
    <Card title="Browser push on this device">
      {!pushSupported() ? (
        <p className="text-sm text-muted">This browser doesn't support web push. On an iPhone or iPad, add swarmageddon to the home screen, then open it from there.</p>
      ) : (
        <div className="flex flex-wrap items-center gap-3">
          <Badge tone={subscribed ? "ok" : "muted"}>{subscribed === undefined ? "checking" : subscribed ? "On" : "Off"}</Badge>
          <Button variant={subscribed ? "secondary" : "primary"} busy={toggle.isPending} onClick={() => toggle.mutate()} disabled={subscribed === undefined}>
            {subscribed ? "Turn off on this device" : "Enable browser push"}
          </Button>
          <p className="w-full text-xs text-muted">Approvals open directly from the notification.</p>
        </div>
      )}
      <ErrorText error={toggle.error} />
    </Card>
  );
}
