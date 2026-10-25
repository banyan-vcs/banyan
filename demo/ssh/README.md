# Demo SSH keys — never reuse anywhere

One throwaway ed25519 keypair per demo user (alice / bob / admin). The
public halves are embedded in `demo/auth-demo.yaml` (`auth.keys`); use the
private halves against the Docker demo's SSH transport (docs/04 §8):

```bash
git clone ssh://alice@localhost:8102/r/demo.git \
  -c core.sshCommand="ssh -i demo/ssh/alice -o IdentitiesOnly=yes \
     -o StrictHostKeyChecking=accept-new"
```

These keys exist only so the demo works with zero setup. They are public
knowledge; any real deployment generates its own keys.
