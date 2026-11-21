// SSHKeysPage.tsx — the settings area's SSH Keys section
// (/settings/ssh-keys): paste one authorized_keys line, see the table
// (type, fingerprint, comment, added), remove your own. The keys
// authenticate the git-over-SSH transport — `git clone
// ssh://git@<host>/<repo>.git` — and only ever their owner (one key, one
// account). The key material never echoes back; the fingerprint names
// each key.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import { addSSHKey, deleteSSHKey, getSSHKeys } from "../api/sshkeys";
import { useT } from "../i18n";
import { ErrorBox, Spinner, whenStr } from "../components/ui";

export function SSHKeysPage(): ReactNode {
  const t = useT();
  const qc = useQueryClient();
  const [line, setLine] = useState("");
  const [error, setError] = useState("");

  const keys = useQuery({
    queryKey: ["ssh-keys"],
    queryFn: getSSHKeys,
    retry: false,
  });

  const add = useMutation({
    mutationFn: () => addSSHKey(line.trim()),
    onSuccess: () => {
      setLine("");
      setError("");
      qc.invalidateQueries({ queryKey: ["ssh-keys"] });
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });
  const remove = useMutation({
    mutationFn: (fingerprint: string) => deleteSSHKey(fingerprint),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["ssh-keys"] }),
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  return (
    <>
      <h1>{t("SSH Keys")}</h1>
      <p className="muted">{t("keys here authenticate the git-over-SSH transport — clone as ssh://git@<host>/<repo>.git; a key only ever names its owner.")}</p>
      {error && <div className="error-box">{error}</div>}
      <form
        className="key-form"
        onSubmit={(e) => {
          e.preventDefault();
          setError("");
          add.mutate();
        }}
      >
        <textarea
          className="key-input"
          rows={2}
          spellCheck={false}
          placeholder={t("ssh-ed25519 AAAA… comment")}
          value={line}
          onChange={(e) => setLine(e.target.value)}
        />
        <button className="btn" type="submit" disabled={add.isPending || line.trim() === ""}>
          {t("add key")}
        </button>
      </form>
      {keys.isPending && <Spinner />}
      {keys.isError && <ErrorBox error={keys.error} />}
      {keys.isSuccess && (keys.data.keys || []).length === 0 && <p className="muted pad">{t("no keys yet.")}</p>}
      {keys.isSuccess && (keys.data.keys || []).length > 0 && (
        <table>
          <thead>
            <tr>
              <th>{t("Fingerprint")}</th>
              <th>{t("Type")}</th>
              <th>{t("Comment")}</th>
              <th>{t("Added")}</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {(keys.data.keys || []).map((k) => (
              <tr key={k.fingerprint}>
                <td>
                  <code>{k.fingerprint}</code>
                </td>
                <td>{k.type}</td>
                <td>{k.comment || <span className="muted">—</span>}</td>
                <td>{whenStr(k.addedAt)}</td>
                <td>
                  <button
                    className="btn ghost danger"
                    onClick={() => {
                      if (window.confirm(t("delete key {0}?", k.fingerprint))) remove.mutate(k.fingerprint);
                    }}
                  >
                    {t("delete")}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
