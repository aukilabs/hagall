# Deployment

- [Binary](#binary)
- [Docker](#docker)
- [Docker Compose](#docker-compose)
- [Kubernetes](#kubernetes)

## Binary

> **_NOTE:_** If you choose to use this deployment method, you need to set up your own HTTPS web server or reverse proxy with an SSL certificate. Hagall listens for incoming connections on port 4000 by default, but this is changeable, see the configuration options above.

### Currently supported platforms

We build pre-compiled binaries for these operating systems and architectures:

- Windows x86, x86_64
- macOS x86_64, ARM64 (M1)
- Linux x86, x86_64, ARM, ARM64
- FreeBSD x86, x86_64
- Solaris x86_64

> **_NOTE:_** Auki Labs doesn't test all of these platforms actively. Windows, FreeBSD and Solaris builds are currently experimental. We don't guarantee that everything works but feel free to reach out with your test results.

1. Download the latest Hagall from [GitHub](https://github.com/aukilabs/hagall/releases).
2. Generate an Ethereum-compatible wallet and put its private key in a file called `hagall-private.key` like the example above.
3. Run it with `./hagall --public-endpoint=https://hagall.example.com --private-key-file hagall-private.key`
4. Expose it using your own reverse proxy with an SSL certificate.

We recommend you use a daemon manager such as systemd, launchd, SysV Init, runit or Supervisord, to make sure Hagall stays running at all times.

Make sure that your web server or reverse proxy has support for WebSockets. If you choose to use nginx, this configuration is needed to enable WebSocket support for the Hagall upstream:

```text
proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection "upgrade";
```

## Docker

Hagall is available on [Docker Hub](https://hub.docker.com/r/aukilabs/hagall).

Here's an example of how to run it:

```shell
docker run --name=hagall --restart=unless-stopped --detach --mount "type=bind,source=$(pwd)/hagall-private.key,target=/hagall-private.key,readonly" -e HAGALL_PUBLIC_ENDPOINT=https://hagall.example.com -e HAGALL_PRIVATE_KEY_FILE=/hagall-private.key -p 4000:4000 aukilabs/hagall:stable
```

Hagall listens for incoming traffic on port 4000 by default. The port can be changed by
changing Hagall's [configuration](configuration.md) or by simply changing the publish
(`-p`) argument in the `docker run` command.

We also recommend you to configure Docker to start automatically with your operating system. Using `--restart=unless-stopped` in your `docker run` command will start Hagall automatically after the Docker daemon has started.

### Supported tags

_See the full list on [Docker Hub](https://hub.docker.com/r/aukilabs/hagall)._

- `latest` (bleeding edge, not recommended)
- `stable` (latest stable version, recommended)
- `v0` (specific major version)
- `v0.5` (specific minor version)
- `v0.5.0` (specific patch version)

### Upgrading

If you're using a non-version specific tag (`stable` or `latest`) or if the version tag you use matches the new version of Hagall you want to upgrade to, simply run `docker pull aukilabs/hagall:stable` (where `stable` is the tag you use) and then restart your container with `docker restart hagall` (if `hagall` is the name of your container).

If you're using a version-specific tag and the new version of Hagall you want to upgrade to doesn't match the tag you use, you need to first change the tag you use and then restart your container. (`v0` matches any v0.x.x version, `v0.5` matches any v0.5.x version, and so on.)

## Docker Compose

Since Hagall needs to be exposed with an HTTPS address and Hagall itself doesn't terminate HTTPS, instead of using the pure Docker setup as described above, we recommend you to use our Docker Compose file that sets up an `nginx-proxy` container that terminates HTTPS and a `letsencrypt` container that obtains a free Let's Encrypt SSL certificate alongside Hagall.

1. Configure your domain name to point to your externally exposed public IP address and configure any firewalls and port forwarding rules to allow incoming traffic to ports 80 and 443.
2. Download the latest Docker Compose YAML file from [GitHub](https://github.com/aukilabs/hagall/blob/main/docker-compose.yml).
3. Configure the environment variables to your liking (you must at least set `VIRTUAL_HOST`, `LETSENCRYPT_HOST` and `HAGALL_PUBLIC_ENDPOINT`, set these to the domain name you configured in step 1).
4. Generate an Ethereum-compatible wallet and put its private key in a file called `hagall-private.key` like in the example above; alternatively, see this guide on [how to generate a wallet with MetaMask](https://www.posemesh.org/hagall-upgrade-guide).
5. With the YAML file in the same folder, start the containers using Docker Compose: `docker-compose up -d`

Just as with the pure Docker setup, we recommend you configure Docker to start automatically with your operating system. If you use our standard Docker Compose YAML file, the containers will start automatically after the Docker daemon has started.

### Upgrading

You can do the same steps as for Docker, but if you're not already running Hagall or you have modified the `docker-compose.yml` file recently and want to deploy the changes, you can navigate to the folder where you have your `docker-compose.yml` file and then run `docker-compose pull` followed by `docker-compose down` and `docker-compose up -d`.

Note that the `docker-compose pull` command will also upgrade the other containers defined in `docker-compose.yml` such as the nginx proxy and the Let's Encrypt helper.

## Kubernetes

Auki provides a Helm chart for running Hagall in Kubernetes. We recommend that you use this Helm chart rather than writing your own Kubernetes manifests. For more information about [what Helm is](https://helm.sh/docs/topics/architecture/) and how to [install](https://helm.sh/docs/intro/install/) it, see Helm's official website.

### Requirements

* Kubernetes 1.14+
* Helm 3
* An HTTPS and WebSocket compatible ingress controller with an SSL certificate that has already been configured

### Installing

The chart can be deployed by CI/CD tools such as ArgoCD or Flux or it can be deployed using Helm on the command line like this:

```shell
helm repo add aukilabs https://charts.aukiverse.com
helm install hagall aukilabs/hagall --set config.HAGALL_PUBLIC_ENDPOINT=https://hagall.example.com --set-file secrets.privateKey=hagall-private.key
```

### Uninstalling

To uninstall (delete) the `hagall` deployment:

```shell
helm delete hagall
```

### Values

Please see [values.yaml](https://github.com/aukilabs/helm-charts/blob/main/charts/hagall/values.yaml) for the available values and their defaults.

Values can be overridden either by using a values file (the `-f` or `--values` flags) or by setting them on the command line using the `--set` flag. For more information, see the official [documentation](https://helm.sh/docs/helm/helm_install/).

You must at least set the `config.HAGALL_PUBLIC_ENDPOINT` key for server registration to work. But depending on which ingress controller you use, you need to set `ingress.enabled=true`, `ingress.hosts[0].host=hagall.example.com` and so on. You also need to configure a secret containing the private key of your Hagall-exclusive wallet, one per Hagall server, either using an existing secret inside Kubernetes or by passing the wallet as a file, letting the chart create the Kubernetes secret for you.

### Upgrading

We recommend you change to use `image.pullPolicy: Always` if you use a non-specific version tag like `stable`/`v0`/`v0.5` (configured by changing the `image.tag` value of the Helm chart) or choose to use a specific version tag like `v0.5.0`. Check *Supported tags* or the *Tags* tab on [Docker Hub](https://hub.docker.com/r/aukilabs/hagall) for the tags you can use.
