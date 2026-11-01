# Two machines on one network. "printer" publishes its name with avahi;
# "client" runs avahi2dns and asks it with dig.
{ module }:
{
  name = "avahi2dns";

  defaults = {
    services.avahi = {
      enable = true;
      ipv6 = true;
    };
  };

  nodes = {
    printer = {
      services.avahi.publish = {
        enable = true;
        addresses = true;
      };
    };

    client =
      { pkgs, ... }:
      {
        imports = [ module ];
        services.avahi2dns.enable = true;
        environment.systemPackages = [ pkgs.dig ];
      };
  };

  testScript = ''
    import re

    start_all()
    printer.wait_for_unit("avahi-daemon.service")
    client.wait_for_unit("avahi2dns.service")


    def dig(name, qtype):
        out = client.succeed(f"dig -p 5354 @127.0.0.1 +tries=1 +time=4 {name} {qtype}")
        match = re.search(r"status: (\w+)", out)
        assert match, out
        status = match.group(1)
        answers = re.findall(rf"^{re.escape(name)}\.\s+\d+\s+IN\s+{qtype}\s+(\S+)", out, re.M | re.I)
        return status, answers


    def addresses(family, scope):
        out = printer.succeed(f"ip -{family} -o addr show dev eth1 scope {scope}")
        return [line.split()[3].split("/")[0] for line in out.splitlines()]


    printer_ipv4 = addresses(4, "global")
    # The test driver gives each machine a global IPv6 address.
    printer_ipv6 = addresses(6, "global")
    assert printer_ipv6, "the printer has no global IPv6 address"

    with subtest("A record of a host"):
        # avahi of the printer needs some time to announce its name.
        client.wait_until_succeeds("dig -p 5354 @127.0.0.1 printer.local A | grep -q 'status: NOERROR'")
        assert dig("printer.local", "A") == ("NOERROR", printer_ipv4)

    with subtest("AAAA record of a host, and the case of the name"):
        # avahi gives one address. It is global: avahi2dns does not send
        # link-local addresses.
        client.wait_until_succeeds("dig -p 5354 @127.0.0.1 printer.local AAAA | grep -q 'IN.AAAA'")
        status, answers = dig("PRINTER.local", "AAAA")
        assert status == "NOERROR" and len(answers) == 1 and answers[0] in printer_ipv6, (status, answers)

    with subtest("other type of an existing host is NODATA"):
        assert dig("printer.local", "TXT") == ("NOERROR", [])

    with subtest("unknown host is NXDOMAIN"):
        assert dig("ghost.local", "A") == ("NXDOMAIN", [])

    with subtest("name in another domain is REFUSED"):
        assert dig("example.com", "A") == ("REFUSED", [])

    with subtest("avahi not running is SERVFAIL"):
        # Mask avahi: else D-Bus starts it again (bus activation).
        client.succeed("systemctl mask --runtime avahi-daemon.service avahi-daemon.socket")
        client.succeed("systemctl stop avahi-daemon.service avahi-daemon.socket")
        assert dig("printer.local", "A") == ("SERVFAIL", [])
  '';
}
