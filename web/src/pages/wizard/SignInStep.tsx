import { useState } from "react";
import type { OIDCProvider, Role, RoleMapping, SAMLProvider } from "@/api/types";
import { Button, Card, Checkbox, Field, Input, Select, Textarea } from "@/components/ui";
import { providerLoginURL } from "@/lib/auth";
import type { StepProps } from "./PlatformWizard";

const newOIDC = (): OIDCProvider => ({ name: "", displayName: "", issuer: "", clientID: "", groupsClaim: "groups", scopes: ["openid", "email", "profile"] });
const newSAML = (): SAMLProvider => ({ name: "", displayName: "", metadataURL: "", metadataXML: "", groupsAttribute: "groups" });

function update<T>(list: T[], i: number, patch: Partial<T>): T[] {
  return list.map((x, j) => (j === i ? { ...x, ...patch } : x));
}

/** Providers' names appear in URLs, so they're DNS-label-like. */
const namePattern = "[a-z0-9]([a-z0-9-]*[a-z0-9])?";

export function SignInStep({ platform, save, saving }: StepProps) {
  const spec = platform.spec;
  const [oidc, setOIDC] = useState(spec.oidc);
  const [saml, setSAML] = useState(spec.saml);
  const [secrets, setSecrets] = useState<Record<string, string>>({});
  const [mappings, setMappings] = useState<RoleMapping[]>(spec.roleMappings);
  const [defaultRole, setDefaultRole] = useState<Role | "">(spec.defaultRole);
  const [forgejoOIDC, setForgejoOIDC] = useState(spec.services.forgejoOIDCLogin);
  const [localDisabled, setLocalDisabled] = useState(spec.localDisabled);

  const next = {
    ...spec,
    oidc,
    saml,
    roleMappings: mappings.filter((m) => m.group),
    defaultRole,
    localDisabled,
    services: { ...spec.services, forgejoOIDCLogin: forgejoOIDC },
  };
  const secretsToSave = { oidcClientSecrets: Object.fromEntries(Object.entries(secrets).filter(([, v]) => v)) };
  const hasIdP = oidc.length + saml.length > 0;

  const test = async (kind: "oidc" | "saml", name: string) => {
    await save(next, undefined, secretsToSave);
    window.open(providerLoginURL({ kind, name }, "/wizard", true), "_blank", "noopener");
  };

  return (
    <form
      className="space-y-4"
      onSubmit={(e) => {
        e.preventDefault();
        void save(next, "services", secretsToSave);
      }}
    >
      <Card title="Local accounts">
        <p className="text-sm text-muted">The local admin you created at setup can always sign in unless you turn local accounts off.</p>
        <Checkbox
          label="Turn off local accounts once an identity provider works"
          checked={localDisabled}
          disabled={!hasIdP}
          onChange={(e) => setLocalDisabled(e.target.checked)}
        />
      </Card>

      <Card title="OIDC providers" actions={<Button onClick={() => setOIDC([...oidc, newOIDC()])}>Add OIDC provider</Button>}>
        {oidc.length === 0 && <p className="text-sm text-muted">None. Keycloak, Entra ID, Google, Okta and Authentik all work.</p>}
        {oidc.map((p, i) => (
          <fieldset key={i} className="mb-4 grid gap-3 rounded-xs border border-border p-3 sm:grid-cols-2">
            <legend className="px-1 text-sm font-medium">OIDC provider {i + 1}</legend>
            <Field label="Name (in URLs)">{(id) => <Input id={id} value={p.name} pattern={namePattern} required onChange={(e) => setOIDC(update(oidc, i, { name: e.target.value }))} />}</Field>
            <Field label="Display name">{(id) => <Input id={id} value={p.displayName} onChange={(e) => setOIDC(update(oidc, i, { displayName: e.target.value }))} />}</Field>
            <Field label="Issuer URL" className="sm:col-span-2">
              {(id) => <Input id={id} type="url" value={p.issuer} required onChange={(e) => setOIDC(update(oidc, i, { issuer: e.target.value }))} />}
            </Field>
            <Field label="Client ID">{(id) => <Input id={id} value={p.clientID} required onChange={(e) => setOIDC(update(oidc, i, { clientID: e.target.value }))} />}</Field>
            <Field label="Client secret" hint="Stored in a Secret; leave empty to keep the current one.">
              {(id, d) => (
                <Input id={id} aria-describedby={d} type="password" autoComplete="off" value={secrets[p.name] ?? ""} onChange={(e) => setSecrets({ ...secrets, [p.name]: e.target.value })} />
              )}
            </Field>
            <Field label="Groups claim">{(id) => <Input id={id} value={p.groupsClaim} onChange={(e) => setOIDC(update(oidc, i, { groupsClaim: e.target.value }))} />}</Field>
            <Field label="Scopes" hint="Space separated.">
              {(id, d) => (
                <Input id={id} aria-describedby={d} value={p.scopes.join(" ")} onChange={(e) => setOIDC(update(oidc, i, { scopes: e.target.value.split(/\s+/).filter(Boolean) }))} />
              )}
            </Field>
            <p className="text-xs text-muted sm:col-span-2">
              Redirect URI to register: <code>{`${location.origin}/auth/oidc/${p.name || "<name>"}/callback`}</code>
            </p>
            <div className="flex gap-2 sm:col-span-2">
              <Button disabled={!p.name || !p.issuer} onClick={() => test("oidc", p.name)}>
                Test sign-in
              </Button>
              <Button variant="ghost" onClick={() => setOIDC(oidc.filter((_, j) => j !== i))}>
                Remove
              </Button>
            </div>
          </fieldset>
        ))}
      </Card>

      <Card title="SAML providers" actions={<Button onClick={() => setSAML([...saml, newSAML()])}>Add SAML provider</Button>}>
        {saml.length > 0 && (
          <p className="mb-3 text-sm">
            Our service provider metadata, to give your IdP:{" "}
            <a className="text-accent underline" href={platform.spMetadataURL} target="_blank" rel="noreferrer">
              {new URL(platform.spMetadataURL, location.origin).href}
            </a>
          </p>
        )}
        {saml.map((p, i) => (
          <fieldset key={i} className="mb-4 grid gap-3 rounded-xs border border-border p-3 sm:grid-cols-2">
            <legend className="px-1 text-sm font-medium">SAML provider {i + 1}</legend>
            <Field label="Name (in URLs)">{(id) => <Input id={id} value={p.name} pattern={namePattern} required onChange={(e) => setSAML(update(saml, i, { name: e.target.value }))} />}</Field>
            <Field label="Display name">{(id) => <Input id={id} value={p.displayName} onChange={(e) => setSAML(update(saml, i, { displayName: e.target.value }))} />}</Field>
            <Field label="IdP metadata URL" className="sm:col-span-2">
              {(id) => <Input id={id} type="url" value={p.metadataURL} onChange={(e) => setSAML(update(saml, i, { metadataURL: e.target.value }))} />}
            </Field>
            <Field label="…or IdP metadata XML" className="sm:col-span-2">
              {(id) => <Textarea id={id} rows={4} value={p.metadataXML} onChange={(e) => setSAML(update(saml, i, { metadataXML: e.target.value }))} />}
            </Field>
            <Field label="Groups attribute">{(id) => <Input id={id} value={p.groupsAttribute} onChange={(e) => setSAML(update(saml, i, { groupsAttribute: e.target.value }))} />}</Field>
            <div className="flex items-end gap-2">
              <Button disabled={!p.name || !(p.metadataURL || p.metadataXML)} onClick={() => test("saml", p.name)}>
                Test sign-in
              </Button>
              <Button variant="ghost" onClick={() => setSAML(saml.filter((_, j) => j !== i))}>
                Remove
              </Button>
            </div>
          </fieldset>
        ))}
      </Card>

      {hasIdP && (
        <Card title="Role mapping" actions={<Button onClick={() => setMappings([...mappings, { group: "", role: "viewer" }])}>Add mapping</Button>}>
          <p className="mb-2 text-sm text-muted">Which identity-provider groups get which role. The highest matching role wins.</p>
          <ul className="space-y-2">
            {mappings.map((m, i) => (
              <li key={i} className="flex flex-wrap items-end gap-2">
                <Field label="Group" className="min-w-40 flex-1">
                  {(id) => <Input id={id} value={m.group} onChange={(e) => setMappings(update(mappings, i, { group: e.target.value }))} />}
                </Field>
                <Field label="Role">
                  {(id) => (
                    <Select id={id} value={m.role} onChange={(e) => setMappings(update(mappings, i, { role: e.target.value as Role }))}>
                      <option value="admin">admin</option>
                      <option value="operator">operator</option>
                      <option value="viewer">viewer</option>
                    </Select>
                  )}
                </Field>
                <Button variant="ghost" aria-label={`Remove mapping ${m.group}`} onClick={() => setMappings(mappings.filter((_, j) => j !== i))}>
                  Remove
                </Button>
              </li>
            ))}
          </ul>
          <Field label="People no mapping matches" className="mt-3 max-w-xs">
            {(id) => (
              <Select id={id} value={defaultRole} onChange={(e) => setDefaultRole(e.target.value as Role | "")}>
                <option value="viewer">get the viewer role</option>
                <option value="">are refused</option>
              </Select>
            )}
          </Field>
          {oidc.length > 0 && (
            <Checkbox className="mt-2" label="Also use the first OIDC provider to sign in to Forgejo" checked={forgejoOIDC} onChange={(e) => setForgejoOIDC(e.target.checked)} />
          )}
        </Card>
      )}

      <div className="flex justify-end gap-2">
        <Button type="submit" variant="primary" busy={saving}>
          {hasIdP ? "Save and continue" : "Continue with local accounts only"}
        </Button>
      </div>
    </form>
  );
}
