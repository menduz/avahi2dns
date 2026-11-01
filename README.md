# avahi2dns [![Build](https://github.com/LouisBrunner/avahi2dns/actions/workflows/build.yml/badge.svg)](https://github.com/LouisBrunner/avahi2dns/actions/workflows/build.yml)

Linux DNS-resolution is done by the standard library (through functions like `getaddrinfo` or `gethostbyname`), unfortunately `musl` (used by Alpine Linux) doesn't support mdns out-of-the-box.

`avahi2dns` is a small DNS server which interfaces with avahi (through D-Bus) and forwards (perfect for Alpine Linux and musl)

## Building

Requires go 1.26 or later

```bash
go build
```

## Usage

```shell
$ ./avahi2dns -h
Usage: avahi2dns [--domains DOMAINS] [--addr ADDR] [--port PORT] [--debug] [--v4only] [--v6only] [--timeout TIMEOUT]

Options:
  --domains DOMAINS, -d DOMAINS
                         list of domains to resolve (e.g. '-d local -d home')
  --addr ADDR, -a ADDR   address to bind on [default: localhost, env: BIND]
  --port PORT, -p PORT   port to bind on [default: 53, env: PORT]
  --debug, -v            also include debug information [default: false]
  --v4only, -4           only resolve A records [default: false]
  --v6only, -6           only resolve AAAA records [default: false]
  --timeout TIMEOUT, -t TIMEOUT
                         timeout for the Avahi request, after which the host is not found, 0 meaning none, see https://pkg.go.dev/time#ParseDuration for units and format [default: 1500ms]
  --help, -h             display this help and exit
```

> [!TIP]
> You can also configure the server through environment variables.

### Answers

For each query, `avahi2dns` looks up the name in IPv4 and IPv6 at the same time (only one of them with `--v4only` or `--v6only`):

- `A`/`AAAA`: the address, as soon as avahi finds one of this family
- the host exists, but has no address of the asked family (or the asked type is not `A`/`AAAA`): `NOERROR` without an answer (NODATA)
- avahi finds the host in no family: `NXDOMAIN`
- avahi fails (e.g. it does not run): `SERVFAIL`

Negative answers include the SOA record of the domain. A link-local IPv6 address (`fe80::/10`) is never sent: it needs the interface, which DNS cannot carry.

Concurrent queries for the same name share one avahi request, e.g. the `A` and `AAAA` queries of one client, or the retransmissions of a resolver.

avahi waits 5 seconds before it reports a missing host. Most resolvers do not wait that long: e.g. Unbound (1.21 and later) drops replies after its `discard-timeout` (1.9 seconds). Thus `--timeout` defaults to 1.5 seconds, after which the host is not found. Avahi sends its mDNS query again after about 1 second, thus 1.5 seconds include one retransmission.

### Examples

By default the server will bind to port 53 on localhost (not accessible outside your computer)
and resolve any domain with the following extensions: `home`, `internal`, `intranet`, `lan`, `local`, `private`, `test`

```shell
$ sudo ./avahi2dns
INFO[0000] starting DNS server                           addr="localhost:53"
...
```

Settings can be changed through command-line arguments or environment variables:

```shell
$ ./avahi2dns -p 5454 -a '0.0.0.0' -d local -d home
or
$ BIND='0.0.0.0' PORT=5454 ./avahi2dns -d local -d home
```

You can also use the debug flag if you need more information about what the server is doing (probably overkill):

```shell
$ ./avahi2dns -v -p 5454
DEBU[2025-04-07T23:27:41Z]/src/server.go:31 main.NewForwarder() connection to dbus...
DEBU[2025-04-07T23:27:41Z]/src/server.go:37 main.NewForwarder() connection to avahi through dbus...
DEBU[2025-04-07T23:27:41Z]/src/server.go:44 main.NewForwarder() adding dns handler                            domain=home
...
```

In this example, `avahi2dns` is run as a background job for the sake of demonstration, don't do that in production (use OpenRC, systemd or similar).

```shell
$ sudo ./avahi2dns &
INFO[0000] starting DNS server                           addr="localhost:53"
$ docker run --rm --entrypoint ash --dns=127.0.0.1 --net=host alpine -c 'apk add iputils && ping your-name-host.local'
fetch https://dl-cdn.alpinelinux.org/alpine/v3.14/main/armv7/APKINDEX.tar.gz
fetch https://dl-cdn.alpinelinux.org/alpine/v3.14/community/armv7/APKINDEX.tar.gz
(1/2) Installing libcap (2.50-r0)
(2/2) Installing iputils (20210202-r0)
Executing busybox-1.33.1-r3.trigger
OK: 4 MiB in 16 packages
INFO[0006] forwarding query to avahi                     component=main name=your-name-host.local. protocol=0 type=A
INFO[0006] forwarding query to avahi                     component=main name=your-name-host.local. protocol=1 type=AAAA
PING your-name-host.local (172.16.16.2) 56(84) bytes of data.
64 bytes from 172.16.16.2 (172.16.16.2): icmp_seq=1 ttl=64 time=0.456 ms
64 bytes from 172.16.16.2 (172.16.16.2): icmp_seq=2 ttl=64 time=0.426 ms
64 bytes from 172.16.16.2 (172.16.16.2): icmp_seq=3 ttl=64 time=0.429 ms
64 bytes from 172.16.16.2 (172.16.16.2): icmp_seq=4 ttl=64 time=0.418 ms
```

> [!IMPORTANT]
> If you are running `avahi2dns` within Docker, you will need to run your container with the `host` network mode (i.e. `--net=host`) instead of the `bridge` default.

## Installation

### Alpine (edge version)

Feel free to check the Alpine wiki directly for their [recommended setup](https://wiki.alpinelinux.org/wiki/MDNS#Setup_avahi2dns).

> [!NOTE]
> Thanks to [Willow Barraco](https://cv.willowbarraco.fr/en/), `avahi2dns` is available through the `testing` repository of Alpine.

```bash
# all those steps require you to be root

# add the testing repository, skip if you already have it enabled
echo "@testing https://dl-cdn.alpinelinux.org/alpine/edge/testing" >> /etc/apk/repositories

apk add avahi2dns@testing

# if you are using openrc
apk add avahi2dns-openrc@testing
# automatically starts avahi2dns at boot
rc-update add avahi2dns
# start it right now as well
rc-service avahi2dns start
```

### NixOS

The flake has a NixOS module (`nixosModules.default`, in `nix/module.nix`). It builds the package with the nixpkgs of the system:

```nix
{
  inputs.avahi2dns.url = "github:menduz/avahi2dns";

  outputs = { nixpkgs, avahi2dns, ... }: {
    nixosConfigurations.example = nixpkgs.lib.nixosSystem {
      modules = [
        avahi2dns.nixosModules.default
        {
          services.avahi.enable = true;
          services.avahi2dns = {
            enable = true; # listens on 127.0.0.1:5354, resolves .local
            # domains = [ "local" ];
            # timeout = "1500ms";
            # family = "any"; # or "ipv4", "ipv6"
          };
          # e.g. Unbound forwards .local to avahi2dns:
          services.unbound.settings = {
            server = {
              do-not-query-localhost = false;
              domain-insecure = [ "local" ];
              private-domain = [ "local" ];
            };
            forward-zone = [
              {
                name = "local.";
                forward-addr = [ "127.0.0.1@5354" ];
                forward-no-cache = true;
              }
            ];
          };
        }
      ];
    };
  };
}
```

`nix flake check` builds the package (with the Go tests) and runs a NixOS test: two virtual machines, one publishes its name with avahi, the other resolves it with avahi2dns.

### Any Linux

Checkout the [latest release](https://github.com/LouisBrunner/avahi2dns/releases/latest) and download the binary corresponding to your architecture.

```bash
# make sure to match your system architecture
wget https://github.com/LouisBrunner/avahi2dns/releases/latest/download/avahi2dns-linux-arm64
mv avahi2dns-linux-arm64 /usr/bin/avahi2dns
```

You can then setup `avahi2dns` through OpenRC, systemd or similar. Check this [file](openrc/avahi2dns) for an example for the former.
