# Call tracking and failover

**Call tracking works on any cluster today; failover doesn't exist yet.** With call tracking on, each FreeSWITCH node
records every live call leg in a PostgreSQL that the whole fleet shares. Failover
(on the [roadmap](../README.md#roadmap)) will use those records to move a lost node's calls to a spare. This
page covers setting tracking up and what failover still needs.

## What it takes

- **An image with `mod_pgsql`.** drachtio's image leaves it out. The project's own,
  `ghcr.io/spiegela/freeswitch` ([`images/freeswitch`](../images/freeswitch/Dockerfile)), adds it. Pin it by
  digest in `spec.image`, or build it yourself:

  ```sh
  docker build -t <registry>/freeswitch:<tag> images/freeswitch
  ```

- **A PostgreSQL that every edge node can reach.** It holds only live state: rows go when calls end. Each node keeps
  two connections open.
  - **kind and small clusters:** a small instance in the cluster. [`deploy/kind/postgres`](../deploy/kind/postgres/postgres.yaml)
    is a starting point. On a real cluster, set a storage class and size, and take the Secret from your secret store
    instead of the committed kind value.
  - **A managed service,** such as Aurora PostgreSQL, through its writer (cluster) endpoint.
- **[`deploy/postgres/freeswitch-core-db.sql`](../deploy/postgres/freeswitch-core-db.sql) run once in that
  database,** below.
- **Four settings in the FreeSWITCH configuration**, below.

## Prepare the database

**Run [`freeswitch-core-db.sql`](../deploy/postgres/freeswitch-core-db.sql) once, before or after FreeSWITCH first
starts**, as a user allowed to create event triggers (a superuser; on Aurora or RDS, the master user):

```sh
psql "host=<writer endpoint> dbname=freeswitch user=<master user> sslmode=require" -v ON_ERROR_STOP=1 \
  -f deploy/postgres/freeswitch-core-db.sql
```

FreeSWITCH creates its tables with length limits (`varchar(1024)` for a caller's name) that SQLite never enforces
and PostgreSQL does. One call over a limit loses other calls' writes: FreeSWITCH writes the core db in batches of
one transaction each, a refused statement ends its batch, and the batch's COMMIT rolls back everything before it,
never retried. That takes the call tracking failover needs, and the deletes of ended calls, leaving `channels` rows
for calls long gone. The script turns every length-limited column into `text`, now and, through an event trigger,
whenever FreeSWITCH creates or alters a table later (a first start, an upgrade). FreeSWITCH keeps the tables it
finds, so the change lasts. Use a database FreeSWITCH alone uses: the script changes every table in it.

Without the right to create event triggers, leave the trigger out and run `SELECT freeswitch_unbound_columns()`
after FreeSWITCH first starts and after each upgrade. `hack/kind.sh deploy` runs the script on kind, and
`hack/kind.sh bench` fails if PostgreSQL refused any statement during the run (sipbench's `raw-large-messages`
sends a caller name over the limit).

## Configure FreeSWITCH

sipbench's configuration ([`sipbench/freeswitch/conf`](../sipbench/freeswitch/conf/)) already has all four. Each one is
read from the environment, so the same ConfigMaps serve compose (tracking off) and a cluster (tracking on).

1. **Load `mod_pgsql` before the core db opens.** Use [`pre_load_modules.conf.xml`](../sipbench/freeswitch/conf/autoload_configs/pre_load_modules.conf.xml),
   not `modules.conf.xml`, which loads too late:

   ```xml
   <configuration name="pre_load_modules.conf">
     <modules><load module="mod_pgsql"/></modules>
   </configuration>
   ```

2. **Point the core db at PostgreSQL** in [`switch.conf.xml`](../sipbench/freeswitch/conf/autoload_configs/switch.conf.xml).
   An empty value leaves it in SQLite:

   ```xml
   <param name="core-db-dsn" value="$${core_db_dsn}"/>
   ```

3. **Turn on `track-calls` on every SIP profile a call crosses.** Here that's `trunks` and `backend`. A bridged call
   can only be recovered with both legs, so tracking one profile isn't enough:

   ```xml
   <param name="track-calls" value="$${track_calls}"/>
   ```

4. **Read both values from the environment** in [`vars.xml`](../sipbench/freeswitch/conf/vars.xml)
   ([`bin/env.sh`](../sipbench/freeswitch/conf/bin/env.sh) prints a variable, or a default when it's unset):

   ```xml
   <X-PRE-PROCESS cmd="exec-set" data="core_db_dsn=/bin/sh /usr/local/freeswitch/conf/bin/env.sh CORE_DB_DSN"/>
   <X-PRE-PROCESS cmd="exec-set" data="track_calls=/bin/sh /usr/local/freeswitch/conf/bin/env.sh TRACK_CALLS false"/>
   ```

Only the core db moves. Each SIP profile's own tables, such as registrations it holds, stay in the pod's SQLite.
Recovery doesn't need them.

## Configure the `FreeswitchCluster`

Set the image and three variables in `spec.env`, as [`deploy/kind/freeswitchcluster.yaml`](../deploy/kind/freeswitchcluster.yaml)
does:

```yaml
spec:
  image: ghcr.io/spiegela/freeswitch@sha256:…  # images/freeswitch
  env:
    - name: CORE_DB_DSN                         # libpq keywords after pgsql://
      value: "pgsql://host=postgres.telephony.svc.cluster.local dbname=freeswitch user=freeswitch connect_timeout=5"
    - name: PGPASSWORD                          # libpq reads it; keep it out of the DSN
      valueFrom: {secretKeyRef: {name: postgres, key: password}}
    - name: TRACK_CALLS
      value: "true"
```

For Aurora, use the writer endpoint as `host` and add `sslmode=require`.

Things to know:

- **Keep the password out of the DSN.** FreeSWITCH logs the DSN when a connection fails. In `PGPASSWORD` it never
  reaches the logs.
- **FreeSWITCH reads the password only when it starts.** A rotated Secret reaches a node only when its pod restarts,
  so don't let a secret store rotate it on its own schedule. One way is to generate it once (an External Secrets
  password generator with `refreshPolicy: CreatedOnce`).
- **Bring the database up first.** With a DSN set, FreeSWITCH won't start until the database answers. A node that
  can't reach it never becomes ready, so a roll stops there.
- **Once running, a database restart is harmless.** FreeSWITCH logs the lost connection, keeps its calls and
  reconnects by itself. This was checked on kind by restarting PostgreSQL under a live edge. An Aurora failover
  should look the same to FreeSWITCH, but hasn't been checked.
- **The edge runs on the host network.** The operator sets `ClusterFirstWithHostNet`, so Service names resolve. But
  the database sees connections coming from the nodes' own addresses, not pod addresses. A NetworkPolicy or
  security group in front of it must allow the edge nodes' addresses.
- **Set a DSN only on an image that has `mod_pgsql`.** On another image the module fails to load, and FreeSWITCH
  only logs that. After changing images, check the edge's log for a `mod_pgsql` load error.

## Check it

While a call is up, each of its legs is a row in the `recovery` table under its node's name. The rows go when the
call ends:

```sh
kubectl -n telephony exec postgres-0 -- psql -U freeswitch -d freeswitch \
  -c 'select hostname, profile_name, uuid from recovery'
```

The core db's tables (`channels`, `calls`, `registrations`, `recovery`, ...) are listed by `\dt` in the same database.
None should have a length limit left:

```sh
kubectl -n telephony exec postgres-0 -- psql -U freeswitch -d freeswitch -c "select table_name, column_name \
  from information_schema.columns where table_schema = current_schema() and character_maximum_length is not null"
```

PostgreSQL's log should show no refused statements beyond the `create index` ones FreeSWITCH repeats at every start
(`relation ... already exists`, harmless: they run on their own, not in a batch).

## Failover: what tracking is for, and what's left

When a node is lost, the plan is:

1. **Fence it.** Its address is unbound before anything else happens, and can never come back to it. This happens
   automatically only where the pool's fencing is `Authoritative` ([address pools](address-pools.md)).
2. **Move its addresses to a spare.**
3. **Recover its calls on the spare.** FreeSWITCH rebuilds each tracked leg from its row. It sends each far end a
   re-INVITE built from the leg's saved dialog (Contact, Via, route), from the moved address, and bridges the legs
   again.

Not built yet:

- **Don't run `sofia recover` on a live fleet.** As FreeSWITCH 1.10.10 has it, it takes over every row another
  FreeSWITCH wrote (`runtime_uuid != own`), live nodes' calls included, and deletes them. `sofia profile <name>
  recover` narrows it to one profile name. Failover will rely on that: one profile per movable address, so
  recovering a profile takes exactly the lost address's calls. Recovery also needs the spare to have a profile of
  the same name, or it skips the row.
- **In-dialog SIP has to reach the spare.** The far end's BYE and re-INVITE go to the Contact it was given. That is
  the lost node's address unless it moved too.
- **Nothing recovers calls automatically.** FreeSWITCH doesn't recover on startup, and dead-node detection,
  fencing, the address move and the recover call are all still to be built. A lost node's rows stay in
  `recovery` until something recovers them.

Until then, a node's calls drop when it dies: FreeSWITCH ends them at its media timeout, and nothing tells the
backend. New calls go elsewhere, within Kamailio's 5 s failover at first, then with no delay once the dispatcher's
probes have taken the node out. sipbench's `node-lost-mid-call` checks that: it takes a call's node off the network
for two minutes (tc drop filters on the node's own interface) and checks the rest of the fleet keeps taking calls.
It records the lost call as the baseline for failover.

