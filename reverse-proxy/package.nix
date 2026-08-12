# Briard's front door at the VIP, built from the repo's Go module.
# Pure stdlib (crypto/tls + net/http/httputil), but buildGoModule vendors the whole module --
# which pulls in lego -- so this shares the module-wide vendorHash (one
# definition, vendor-hash.nix). Runs in the guest, promoter-owned, serving :80 + :443 at the VIP.
{ buildGoModule }:
buildGoModule {
  pname = "briard-reverse-proxy";
  version = "0.0.0";
  src = ../.;
  vendorHash = import ../vendor-hash.nix;
  subPackages = [ "reverse-proxy" ];
  meta.description = "Briard reverse proxy (serves the VIP, terminates TLS, hot-reloads on renewal)";
}
