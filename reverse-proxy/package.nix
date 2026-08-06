# Briard's front door at the VIP, built from the repo's Go module.
# Pure stdlib (crypto/tls + net/http/httputil), but buildGoModule vendors the whole module --
# which pulls in lego -- so this shares the agent's vendorHash. Regenerate together
# on go.mod/go.sum changes. Runs in the guest, promoter-owned, serving :80 + :443 at the VIP.
{ buildGoModule }:
buildGoModule {
  pname = "briard-reverse-proxy";
  version = "0.0.0";
  src = ../.;
  vendorHash = "sha256-5qbgoM8+XGR9gxMHkq+WqNO9EqTTk+1s9bZ6poMBYQc=";
  subPackages = [ "reverse-proxy" ];
  meta.description = "Briard reverse proxy (serves the VIP, terminates TLS, hot-reloads on renewal)";
}
