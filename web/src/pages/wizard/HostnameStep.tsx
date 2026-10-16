import { Tabs } from "radix-ui";
import { useState } from "react";
import { Button, Card, Field, Input, Select } from "@/components/ui";
import type { StepProps } from "./PlatformWizard";

/** Where the platform CA can be downloaded (served by the control center). */
export const caURL = "/ca.crt";

const trust: { os: string; steps: string[] }[] = [
  {
    os: "macOS",
    steps: [
      "Download the CA certificate.",
      "Open it: Keychain Access adds it to the login keychain.",
      'Double-click "swarmageddon CA", expand Trust, and set "When using this certificate" to Always Trust.',
      "Or in a terminal: sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ca.crt",
    ],
  },
  {
    os: "Windows",
    steps: [
      "Download the CA certificate and open it.",
      'Click "Install Certificate…", choose Local Machine, then "Place all certificates in the following store".',
      'Pick "Trusted Root Certification Authorities" and finish.',
      "Or in an admin PowerShell: Import-Certificate -FilePath ca.crt -CertStoreLocation Cert:\\LocalMachine\\Root",
    ],
  },
  {
    os: "Linux",
    steps: [
      "Debian/Ubuntu: sudo cp ca.crt /usr/local/share/ca-certificates/swarmageddon.crt && sudo update-ca-certificates",
      "Fedora/RHEL: sudo cp ca.crt /etc/pki/ca-trust/source/anchors/swarmageddon.crt && sudo update-ca-trust",
      "Firefox keeps its own store: Settings → Privacy & Security → Certificates → View Certificates → Authorities → Import.",
    ],
  },
  {
    os: "iOS",
    steps: [
      "Open the CA certificate link in Safari and allow the profile download.",
      "Settings → General → VPN & Device Management → install the swarmageddon profile.",
      "Settings → General → About → Certificate Trust Settings → turn on full trust for swarmageddon CA.",
    ],
  },
  {
    os: "Android",
    steps: [
      "Download the CA certificate.",
      "Settings → Security → More security settings → Encryption & credentials → Install a certificate → CA certificate.",
      "Pick the downloaded file. Chrome trusts user CAs; some apps don't.",
    ],
  },
];

export function HostnameStep({ platform, save, saving }: StepProps) {
  const spec = platform.spec;
  const [hostname, setHostname] = useState(spec.hostname);
  const [mode, setMode] = useState(spec.tlsMode);
  const [issuer, setIssuer] = useState(spec.issuerName);
  const cmAvailable = platform.versions["certManager"] !== undefined;
  return (
    <form
      className="space-y-4"
      onSubmit={(e) => {
        e.preventDefault();
        void save({ ...spec, hostname: hostname.trim(), tlsMode: mode, issuerName: issuer }, "workspace");
      }}
    >
      <Card title="Hostname (optional)">
        <p className="mb-3 text-sm text-muted">
          Without a hostname, the control center stays at <code>{platform.url}</code>. With one, Forgejo is served at <code>forge.&lt;hostname&gt;</code>; point DNS
          for both at the node or load balancer.
        </p>
        <div className="grid gap-3 sm:grid-cols-2">
          <Field label="Hostname">
            {(id) => (
              <Input id={id} value={hostname} placeholder="agents.example.com" pattern="^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$" onChange={(e) => setHostname(e.target.value)} />
            )}
          </Field>
          <Field label="Certificate" hint={cmAvailable ? undefined : "cert-manager was not found in the cluster."}>
            {(id, d) => (
              <Select id={id} aria-describedby={d} value={mode} onChange={(e) => setMode(e.target.value as typeof mode)}>
                <option value="selfSigned">swarmageddon's own CA</option>
                <option value="certManager" disabled={!cmAvailable}>
                  cert-manager (ACME or an existing issuer)
                </option>
              </Select>
            )}
          </Field>
          {mode === "certManager" && (
            <Field label="ClusterIssuer name">{(id) => <Input id={id} value={issuer} required onChange={(e) => setIssuer(e.target.value)} />}</Field>
          )}
        </div>
      </Card>

      {mode === "selfSigned" && (
        <Card title="Trust the platform CA">
          <p className="mb-2 text-sm">
            <a className="text-accent underline" href={caURL} download="swarmageddon-ca.crt">
              Download the CA certificate
            </a>{" "}
            <span className="font-mono text-xs break-all text-muted">(fingerprint {platform.caFingerprint})</span>
          </p>
          <Tabs.Root defaultValue={guessOS()}>
            <Tabs.List aria-label="Operating system" className="flex flex-wrap gap-1 border-b border-border">
              {trust.map((t) => (
                <Tabs.Trigger
                  key={t.os}
                  value={t.os}
                  className="min-h-10 px-3 text-sm text-muted data-[state=active]:border-b-2 data-[state=active]:border-accent data-[state=active]:text-fg"
                >
                  {t.os}
                </Tabs.Trigger>
              ))}
            </Tabs.List>
            {trust.map((t) => (
              <Tabs.Content key={t.os} value={t.os} className="pt-3">
                <ol className="list-decimal space-y-1 pl-5 text-sm">
                  {t.steps.map((s) => (
                    <li key={s} className="break-words">
                      {s}
                    </li>
                  ))}
                </ol>
              </Tabs.Content>
            ))}
          </Tabs.Root>
        </Card>
      )}

      <div className="flex justify-end gap-2">
        <Button busy={saving} onClick={() => save(spec, "workspace")}>
          Skip this step
        </Button>
        <Button type="submit" variant="primary" busy={saving}>
          Save and continue
        </Button>
      </div>
    </form>
  );
}

function guessOS(): string {
  const ua = navigator.userAgent;
  if (/iPhone|iPad/.test(ua)) return "iOS";
  if (/Android/.test(ua)) return "Android";
  if (/Mac/.test(ua)) return "macOS";
  if (/Win/.test(ua)) return "Windows";
  return "Linux";
}
