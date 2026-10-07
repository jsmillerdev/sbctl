-- Per-project custom hostnames and vanity subdomains (internal/domains). Range 1190-1219
-- belongs to the custom-domains workstream.
--
-- A custom hostname is claimed by a project (initialize), proven by DNS (reverify) and turned
-- on (activate). Activation inserts the hostname into supavise.routes, which is what the
-- proxy serves and what its certificate gate allows; deactivation deletes that row. Status
-- values are the ones of hosted's custom-hostname API.

create table supavise.custom_hostnames (
  -- One custom hostname per project, as on hosted.
  ref         text        primary key references supavise.projects (ref) on delete cascade,
  hostname    text        not null check (hostname = lower(hostname) and length(hostname) <= 253),
  status      text        not null check (status in (
                '1_not_started', '2_initiated', '3_challenge_verified',
                '4_origin_setup_completed', '5_services_reconfigured')),
  -- Value of the ownership TXT record at _supavise-challenge.<hostname>.
  token       text        not null,
  -- What the last DNS check found: the hostname points at this node, the TXT record is there.
  cname_ok    boolean     not null default false,
  txt_ok      boolean     not null default false,
  created_at  timestamptz not null default now(),
  updated_at  timestamptz not null default now(),
  verified_at timestamptz,
  activated_at timestamptz
);
-- Anyone may start claiming a hostname (nobody can finish without controlling its DNS), but
-- only one project may hold it once it is verified or active.
create unique index custom_hostnames_held_idx on supavise.custom_hostnames (hostname)
  where status in ('4_origin_setup_completed', '5_services_reconfigured');
create index custom_hostnames_hostname_idx on supavise.custom_hostnames (hostname);

-- A vanity subdomain: <name>.api.<domain>, unique across the node.
create table supavise.vanity_subdomains (
  ref        text        primary key references supavise.projects (ref) on delete cascade,
  name       text        not null unique check (name = lower(name) and length(name) between 1 and 63),
  created_at timestamptz not null default now()
);
