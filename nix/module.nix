{
  config,
  lib,
  pkgs,
  ...
}:

let
  inherit (lib)
    escapeShellArgs
    getExe
    literalExpression
    mkEnableOption
    mkIf
    mkOption
    optional
    optionals
    types
    ;

  cfg = config.services.avahi2dns;

  flags = [
    "--addr"
    cfg.address
    "--port"
    (toString cfg.port)
  ]
  ++ lib.concatMap (domain: [
    "--domains"
    domain
  ]) cfg.domains
  ++ optionals (cfg.timeout != null) [
    "--timeout"
    cfg.timeout
  ]
  ++ optional (cfg.family == "ipv4") "--v4only"
  ++ optional (cfg.family == "ipv6") "--v6only"
  ++ optional cfg.debug "--debug"
  ++ cfg.extraFlags;
in
{
  options.services.avahi2dns = {
    enable = mkEnableOption ''
      avahi2dns, a DNS server that resolves the names of some domains
      (e.g. `.local`) with avahi. Use it to give mDNS names to the clients
      that do not use nss-mdns, e.g. a local DNS resolver that forwards
      `local.` to it
    '';

    package = mkOption {
      type = types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = literalExpression "pkgs.callPackage ./package.nix { }";
      description = "The avahi2dns package to use.";
    };

    address = mkOption {
      type = types.str;
      default = "127.0.0.1";
      example = "0.0.0.0";
      description = "The address on which avahi2dns listens (UDP).";
    };

    port = mkOption {
      type = types.port;
      default = 5354;
      example = 53;
      description = ''
        The port on which avahi2dns listens (UDP). A port below 1024 gives
        the service the capability `CAP_NET_BIND_SERVICE`.
      '';
    };

    domains = mkOption {
      type = types.nonEmptyListOf types.str;
      default = [ "local" ];
      example = [
        "local"
        "home.arpa"
      ];
      description = ''
        The domains that avahi2dns resolves. avahi2dns answers `REFUSED` for
        the names in other domains.
      '';
    };

    timeout = mkOption {
      type = types.nullOr types.str;
      default = null;
      example = "1s";
      description = ''
        The time that avahi2dns waits for avahi, in the format of Go
        (`time.ParseDuration`). After this time, the host is not found.
        `null` keeps the default of avahi2dns (1.5 seconds), which is
        shorter than the time after which most resolvers drop the reply
        (e.g. the `discard-timeout` of Unbound, 1.9 seconds).
      '';
    };

    family = mkOption {
      type = types.enum [
        "any"
        "ipv4"
        "ipv6"
      ];
      default = "any";
      description = ''
        The address families that avahi2dns looks up: `ipv4` gives only
        `A` records, `ipv6` gives only `AAAA` records.
      '';
    };

    debug = mkOption {
      type = types.bool;
      default = false;
      description = "Log each request and reply.";
    };

    openFirewall = mkOption {
      type = types.bool;
      default = false;
      description = "Open the UDP port of avahi2dns in the firewall.";
    };

    extraFlags = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = "More command-line flags for avahi2dns.";
    };
  };

  config = mkIf cfg.enable {
    assertions = [
      {
        assertion = config.services.avahi.enable;
        message = "services.avahi2dns needs services.avahi.enable = true.";
      }
    ];

    networking.firewall.allowedUDPPorts = optional cfg.openFirewall cfg.port;

    systemd.services.avahi2dns = {
      description = "DNS server for mDNS names, resolved by avahi";
      # avahi2dns connects to the system bus once, at start.
      requires = [ "dbus.service" ];
      wants = [ "avahi-daemon.service" ];
      after = [
        "network.target"
        "dbus.service"
        "avahi-daemon.service"
      ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        ExecStart = "${getExe cfg.package} ${escapeShellArgs flags}";
        Restart = "on-failure";
        RestartSec = 5;
        DynamicUser = true;

        AmbientCapabilities = optional (cfg.port < 1024) "CAP_NET_BIND_SERVICE";
        # An empty string removes all the capabilities.
        CapabilityBoundingSet = if cfg.port < 1024 then [ "CAP_NET_BIND_SERVICE" ] else "";
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        NoNewPrivileges = true;
        PrivateDevices = true;
        PrivateTmp = true;
        ProtectClock = true;
        ProtectControlGroups = true;
        ProtectHome = true;
        ProtectHostname = true;
        ProtectKernelLogs = true;
        ProtectKernelModules = true;
        ProtectKernelTunables = true;
        ProtectProc = "invisible";
        ProtectSystem = "strict";
        RemoveIPC = true;
        # AF_UNIX: the system bus.
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        SystemCallArchitectures = "native";
        SystemCallErrorNumber = "EPERM";
        SystemCallFilter = [
          "@system-service"
          "~@privileged @resources"
        ];
      };
    };
  };
}
