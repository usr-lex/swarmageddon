import { useState } from "react";
import type { ModelProvider, PlatformServices, ProviderKind } from "@/api/types";
import { Badge, Button, Card, Checkbox, Field, Input, Select, Spinner } from "@/components/ui";
import { serviceProgress } from "@/lib/platform";
import type { StepProps } from "./PlatformWizard";

const defaultModels: Record<ProviderKind, string[]> = {
  anthropic: ["claude-opus-5-5", "claude-sonnet-5"],
  openai: ["gpt-5.5"],
  openaiCompatible: [],
};

const sizePattern = "^[0-9]+(Mi|Gi|Ti)$";

export function ServicesStep({ platform, save, saving }: StepProps) {
  const spec = platform.spec;
  const [svc, setSvc] = useState<PlatformServices>(spec.services);
  const [keys, setKeys] = useState<Record<string, string>>({});
  const set = (patch: Partial<PlatformServices>) => setSvc({ ...svc, ...patch });
  const setProvider = (i: number, patch: Partial<ModelProvider>) =>
    set({ providers: svc.providers.map((p, j) => (j === i ? { ...p, ...patch } : p)) });
  const progress = serviceProgress({ ...spec, services: svc }, platform.conditions);
  const allReady = progress.every((p) => p.state === "ready");
  const approved = spec.servicesApproved;
  const missingKey = svc.providers.some((p) => p.kind !== "openaiCompatible" && !p.hasKey && !keys[p.name]);

  if (approved) {
    return (
      <div className="space-y-4">
        <Card title="Deploying platform services">
          <ol className="divide-y divide-border" aria-label="Service progress">
            {progress.map((p) => (
              <li key={p.condition} className="flex items-center justify-between gap-3 py-2" data-testid={`service-${p.condition}`} data-state={p.state}>
                <div>
                  <p className="text-sm font-medium">{p.label}</p>
                  {p.message && <p className="text-xs text-muted">{p.message}</p>}
                </div>
                {p.state === "ready" && <Badge tone="ok">Ready</Badge>}
                {p.state === "progress" && <Spinner label="Deploying" />}
                {p.state === "waiting" && <Badge>Waiting</Badge>}
                {p.state === "failed" && <Badge tone="bad">Failed</Badge>}
              </li>
            ))}
          </ol>
        </Card>
        <div className="flex justify-end">
          <Button variant="primary" disabled={!allReady} busy={saving} onClick={() => save(spec, "hostname")}>
            {allReady ? "Continue" : "Waiting for services…"}
          </Button>
        </div>
      </div>
    );
  }

  return (
    <form
      className="space-y-4"
      onSubmit={(e) => {
        e.preventDefault();
        void save({ ...spec, services: svc, servicesApproved: true }, undefined, {
          providerKeys: Object.fromEntries(Object.entries(keys).filter(([, v]) => v)),
        });
      }}
    >
      <Card
        title="Model providers"
        actions={
          <Button onClick={() => set({ providers: [...svc.providers, { name: "", kind: "anthropic", baseURL: "", models: defaultModels.anthropic, hasKey: false }] })}>
            Add provider
          </Button>
        }
      >
        <p className="mb-3 text-sm text-muted">Keys are stored in a Secret only the model gateway mounts. They never enter rooms.</p>
        {svc.providers.length === 0 && <p className="text-sm text-bad">Add at least one provider so agents can reach a model.</p>}
        {svc.providers.map((p, i) => (
          <fieldset key={i} className="mb-3 grid gap-3 rounded-md border border-border p-3 sm:grid-cols-2">
            <legend className="px-1 text-sm font-medium">Provider {i + 1}</legend>
            <Field label="Kind">
              {(id) => (
                <Select id={id} value={p.kind} onChange={(e) => {
                  const kind = e.target.value as ProviderKind;
                  setProvider(i, { kind, models: defaultModels[kind], name: p.name || kind });
                }}>
                  <option value="anthropic">Anthropic</option>
                  <option value="openai">OpenAI</option>
                  <option value="openaiCompatible">OpenAI-compatible (local or proxy)</option>
                </Select>
              )}
            </Field>
            <Field label="Name">{(id) => <Input id={id} value={p.name} required pattern="[a-z0-9-]+" onChange={(e) => setProvider(i, { name: e.target.value })} />}</Field>
            {p.kind === "openaiCompatible" && (
              <Field label="Base URL" className="sm:col-span-2">
                {(id) => <Input id={id} type="url" required value={p.baseURL} onChange={(e) => setProvider(i, { baseURL: e.target.value })} />}
              </Field>
            )}
            <Field label="API key" hint={p.hasKey ? "A key is stored; leave empty to keep it." : p.kind === "openaiCompatible" ? "Optional for local servers." : undefined}>
              {(id, d) => (
                <Input id={id} aria-describedby={d} type="password" autoComplete="off" value={keys[p.name] ?? ""} onChange={(e) => setKeys({ ...keys, [p.name]: e.target.value })} />
              )}
            </Field>
            <Field label="Models" hint="Comma separated, as the gateway will name them.">
              {(id, d) => (
                <Input
                  id={id}
                  aria-describedby={d}
                  value={p.models.join(", ")}
                  onChange={(e) => setProvider(i, { models: e.target.value.split(",").map((m) => m.trim()).filter(Boolean) })}
                />
              )}
            </Field>
            <div className="sm:col-span-2">
              <Button variant="ghost" onClick={() => set({ providers: svc.providers.filter((_, j) => j !== i) })}>
                Remove provider
              </Button>
            </div>
          </fieldset>
        ))}
      </Card>

      <Card title="Services and sizes">
        <div className="grid gap-3 sm:grid-cols-2">
          <Field label="PostgreSQL storage">{(id) => <Input id={id} pattern={sizePattern} value={svc.postgresStorage} onChange={(e) => set({ postgresStorage: e.target.value })} />}</Field>
          <Field label="Forgejo storage">{(id) => <Input id={id} pattern={sizePattern} value={svc.forgejoStorage} onChange={(e) => set({ forgejoStorage: e.target.value })} />}</Field>
          <Field label="Messaging replicas" hint="3 for high availability; 1 on a single node.">
            {(id, d) => (
              <Select id={id} aria-describedby={d} value={svc.messagingReplicas} onChange={(e) => set({ messagingReplicas: Number(e.target.value) })}>
                <option value={0}>automatic</option>
                <option value={1}>1</option>
                <option value={3}>3</option>
              </Select>
            )}
          </Field>
          <Field label="Messaging storage (per replica)">{(id) => <Input id={id} pattern={sizePattern} value={svc.messagingStorage} onChange={(e) => set({ messagingStorage: e.target.value })} />}</Field>
          <Field label="Registry cache storage">{(id) => <Input id={id} pattern={sizePattern} value={svc.registryCacheStorage} onChange={(e) => set({ registryCacheStorage: e.target.value })} />}</Field>
        </div>
        <Checkbox className="mt-3" label="Deploy ntfy for phone push notifications" checked={svc.ntfyEnabled} onChange={(e) => set({ ntfyEnabled: e.target.checked })} />
        {svc.ntfyEnabled && (
          <Field label="ntfy storage" className="max-w-xs">
            {(id) => <Input id={id} pattern={sizePattern} value={svc.ntfyStorage} onChange={(e) => set({ ntfyStorage: e.target.value })} />}
          </Field>
        )}
      </Card>

      <div className="flex justify-end">
        <Button type="submit" variant="primary" busy={saving} disabled={svc.providers.length === 0 || missingKey}>
          Deploy services
        </Button>
      </div>
    </form>
  );
}
