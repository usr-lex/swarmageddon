import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { api } from "@/api";
import { Button, Card, ErrorText, Field, Input } from "@/components/ui";

/** Claiming admin with the setup token (§3.3): one ClaimAdmin call checks the token and creates the admin. */
export function SetupPage() {
  const setup = useQuery({ queryKey: ["setup"], queryFn: () => api().getSetupState() });
  const [token, setToken] = useState("");
  const [username, setUsername] = useState("admin");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const qc = useQueryClient();
  const navigate = useNavigate();
  const done = async () => {
    await qc.invalidateQueries();
    await navigate({ to: "/wizard" });
  };
  const create = useMutation({ mutationFn: () => api().claimAdmin(token, { username, email, password }), onSuccess: done });
  const idp = useMutation({ mutationFn: () => api().claimAdmin(token, null), onSuccess: done });
  const mismatch = confirm.length > 0 && confirm !== password;

  return (
    <div className="mx-auto max-w-lg space-y-4">
      <h1 className="text-2xl font-semibold tracking-tight">Set up swarmageddon</h1>
      {setup.data && (
        <Card title="Check you reached the right server">
          <p className="text-sm text-muted">
            The install printed the CA fingerprint. If your browser shows the same fingerprint for this site's certificate authority, nobody is in
            between.
          </p>
          <p className="mt-2 font-mono text-xs break-all" data-testid="ca-fingerprint">
            {setup.data.caFingerprint}
          </p>
        </Card>
      )}
      <form
        className="space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          if (!mismatch) create.mutate();
        }}
      >
        <Card title="1. Setup token">
          <Field
            label="Setup token"
            hint={
              <>
                Printed by <code>kubectl -n swarmageddon-system logs job/welcome</code>, or read it with{" "}
                <code>kubectl -n swarmageddon-system get secret setup-token -o jsonpath='{"{.data.token}"}' | base64 -d</code>.
              </>
            }
          >
            {(id, d) => (
              <Input id={id} aria-describedby={d} value={token} onChange={(e) => setToken(e.target.value)} autoComplete="off" spellCheck={false} required className="font-mono" />
            )}
          </Field>
        </Card>
        <Card title="2. The first admin">
          <div className="space-y-3">
            <Field label="Username">{(id) => <Input id={id} value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" required />}</Field>
            <Field label="Email">{(id) => <Input id={id} type="email" value={email} onChange={(e) => setEmail(e.target.value)} autoComplete="email" required />}</Field>
            <Field label="Password" hint="At least 12 characters.">
              {(id, d) => (
                <Input id={id} aria-describedby={d} type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" minLength={12} required />
              )}
            </Field>
            <Field label="Confirm password" error={mismatch ? "The passwords differ." : undefined}>
              {(id, d) => (
                <Input id={id} aria-describedby={d} aria-invalid={mismatch} type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="new-password" required />
              )}
            </Field>
          </div>
        </Card>
        <ErrorText error={create.error ?? idp.error} />
        <div className="flex flex-wrap gap-2">
          <Button type="submit" variant="primary" busy={create.isPending} disabled={mismatch}>
            Create admin
          </Button>
          <Button onClick={() => idp.mutate()} busy={idp.isPending} disabled={!token.trim()}>
            Skip: set up OIDC or SAML first
          </Button>
        </div>
        <p className="text-xs text-muted">
          Skipping signs this browser in for setup only; the first person your identity provider maps to the admin role becomes admin.
        </p>
      </form>
    </div>
  );
}
