import { useMutation } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "@/api";
import type { CLI, GitHubDiscovery, WorkspaceDraft } from "@/api/types";
import { Badge, Button, Card, Checkbox, ErrorText, Field, Input, Select } from "@/components/ui";
import { defaultRoom } from "@/lib/workspace";

/**
 * Import a GitHub organization (ADR 0012): its repositories with their
 * issues, pull requests, reviews and comments, and a room for each
 * bot-<name> account, whose posts become that room's. People's posts stay
 * "originally by". The token goes to Forgejo for the import only.
 */
export function GitHubImport({ draft, set }: { draft: WorkspaceDraft; set: (d: WorkspaceDraft) => void }) {
  const [org, setOrg] = useState(draft.org);
  const [token, setToken] = useState("");
  const [prefix, setPrefix] = useState("bot-");
  const [found, setFound] = useState<GitHubDiscovery | null>(null);
  const [repos, setRepos] = useState<Record<string, boolean>>({});
  const [bots, setBots] = useState<Record<string, boolean>>({});
  const [cli, setCli] = useState<CLI>("claude");
  const [added, setAdded] = useState("");
  const discover = useMutation({
    mutationFn: () => api().discoverGitHub(org.trim(), token.trim(), prefix),
    onSuccess: (d) => {
      setFound(d);
      setRepos(Object.fromEntries(d.repos.map((r) => [r.name, !r.archived])));
      setBots(Object.fromEntries(d.bots.map((b) => [b.login, b.room !== ""])));
      setAdded("");
    },
  });

  const add = () => {
    if (!found) return;
    const haveRepo = new Set(draft.repositories.map((r) => r.name));
    const haveRoom = new Set(draft.rooms.map((r) => r.name));
    const newRepos = found.repos
      .filter((r) => repos[r.name] && !haveRepo.has(r.name))
      .map((r) => ({ name: r.name, importURL: `https://github.com/${org.trim()}/${r.name}.git`, importCredential: token.trim(), group: "", pushMirror: "", mirrorToken: "", importGitHub: true }));
    const chosen = found.bots.filter((b) => bots[b.login] && b.room);
    const newRooms = chosen.filter((b) => !haveRoom.has(b.room)).map((b) => ({ ...defaultRoom(b.room, "implementer", draft.defaultModel), cli }));
    const accounts = [...(draft.githubAccounts ?? []).filter((a) => !chosen.some((b) => b.login === a.login)), ...chosen];
    set({ ...draft, repositories: [...draft.repositories, ...newRepos], rooms: [...draft.rooms, ...newRooms], githubAccounts: accounts });
    setAdded(`Added ${newRepos.length} repositories and ${newRooms.length} rooms; ${chosen.length} accounts' history goes to their rooms.`);
  };

  return (
    <Card title="Import from GitHub">
      <p className="mb-3 text-sm text-muted">
        A one-time copy of an organization: repositories with their issues, pull requests, reviews, comments, labels, milestones, releases and wiki. Each <code>{prefix}name</code>{" "}
        account becomes room <code>name</code> and its history becomes the room's; people's posts stay "originally by". Use a read-only fine-grained token; Forgejo uses it for the
        import and it is deleted afterwards.
      </p>
      <div className="grid gap-3 sm:grid-cols-3">
        <Field label="GitHub organization">{(id) => <Input id={id} value={org} onChange={(e) => setOrg(e.target.value)} />}</Field>
        <Field label="Token">{(id) => <Input id={id} type="password" autoComplete="off" value={token} onChange={(e) => setToken(e.target.value)} />}</Field>
        <Field label="Bot account prefix">{(id) => <Input id={id} value={prefix} onChange={(e) => setPrefix(e.target.value)} />}</Field>
      </div>
      <div className="mt-3">
        <Button disabled={!org.trim() || !token.trim()} busy={discover.isPending} onClick={() => discover.mutate()}>
          Look up
        </Button>
      </div>
      <ErrorText error={discover.error} />
      {found && (
        <div className="mt-4 grid gap-4 sm:grid-cols-2" data-testid="github-found">
          <fieldset>
            <legend className="mb-1 text-sm font-medium">Repositories ({found.repos.length})</legend>
            {found.repos.map((r) => (
              <Checkbox
                key={r.name}
                label={
                  <>
                    {r.name} {r.archived && <Badge>archived</Badge>} {r.openIssues > 0 && <span className="text-muted">· {r.openIssues} open issues</span>}
                  </>
                }
                checked={repos[r.name] ?? false}
                onChange={(e) => setRepos({ ...repos, [r.name]: e.target.checked })}
              />
            ))}
          </fieldset>
          <fieldset>
            <legend className="mb-1 text-sm font-medium">
              Rooms from {prefix} accounts ({found.bots.length}); {found.people} people stay "originally by"
            </legend>
            {found.bots.map((b) => (
              <Checkbox
                key={b.login}
                label={b.room ? `${b.login} → room ${b.room}` : `${b.login} (not a valid room name)`}
                disabled={!b.room}
                checked={bots[b.login] ?? false}
                onChange={(e) => setBots({ ...bots, [b.login]: e.target.checked })}
              />
            ))}
            <Field label="CLI for new rooms" hint="Role implementer; change either in the Agents step.">
              {(id, d) => (
                <Select id={id} aria-describedby={d} value={cli} onChange={(e) => setCli(e.target.value as CLI)} className="w-auto">
                  <option value="claude">Claude Code</option>
                  <option value="ore">ore</option>
                </Select>
              )}
            </Field>
          </fieldset>
          <div className="sm:col-span-2">
            <Button variant="primary" onClick={add}>
              Add to the plan
            </Button>
            {added && (
              <p role="status" className="mt-2 text-sm">
                {added}
              </p>
            )}
          </div>
        </div>
      )}
    </Card>
  );
}
