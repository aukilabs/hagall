# Troubleshooting

After launching the Relay server, it's a good idea to take a look at the logs to make sure your server is registered, working and accessible. There are a few things you can look for:

- "hagall is successfully registered to hds" should show up in the log
- `"message":"new client is connected","tags":{"app_key":"0xSMOKE"}}` should show up in the log. These are health checks (also known as smoke tests) running from the central Hagall Discovery Service (HDS) to test that the server is working
- Check metrics like `ws_connected_clients` (see the [Metrics](metrics.md) document for details)
- Check that the server appears on Auki's posemesh
  [dashboard](https://dashboard.auki.network/servers)

If registration to HDS fails, check the status code in the log message. The status code is the response from HDS when it tries to call your Hagall server.
Here are some common issues and solutions:

- `"status":"504 Gateway Timeout"` or `403 Forbidden` could mean that HDS couldn't reach your Relay server because the connection to your public endpoint (URL) timed out. It could happen because you didn't do port forwarding in your router or didn't allow your web server / reverse proxy (such as nginx) or port 443 in your firewall. We have also seen cases where Internet Service Providers blocked common service ports like 80 and 443. Here's an example for [Orcon](https://help.orcon.net.nz/hc/en-us/articles/360005168154-Port-filtering-in-My-Orcon).
- You can test that your Relay server is reachable from the public Internet using [reqbin.com](https://reqbin.com/). Write your Relay server URL (the "public endpoint" address you configured) and press the Send button. If you get a status 400 (Bad Request) back, the text is green and the content says "not websocket protocol", everything is working as it should. If you don't, this is likely the reason for why your server fails to register with HDS.
- `403 Forbidden` could also happen if your clock is out of sync. Sync it and try again, you can use `ntpd`, `chrony` or `ntpdate` on Linux systems and if you need to verify the clock with a website, [time.is](https://time.is) is a good one.
- `403 Forbidden` could also happen if you run two Relay servers at the same time with the same public endpoint (URL). Make sure you only run one Relay server with one unique public endpoint.
- If you're running Docker on Windows and you're seeing an error message like `"token used before issued"`, you might have the wrong timezone inside your container and you might need to forward the `/etc/localtime` file from the Docker VM into the container. In your Docker Compose YAML file, add this under `volumes:` of `hagall:`
  ```
        - /etc/localtime:/etc/localtime:ro
  ```
  Example result:
  ```
    hagall:
      image: aukilabs/hagall:stable
      restart: unless-stopped
      volumes:
        - ./hagall-private.key:/hagall-private.key:ro
        - /etc/localtime:/etc/localtime:ro
  ```
- `503 Service Unavailable` means that HDS reaches a reverse proxy (such as nginx), but the backend server (Relay) is unavailable. Check the reverse proxy logs to verify that HDS actually reaches the reverse proxy you think it reaches. And make sure the Relay server is running and that the reverse proxy points to the Relay server using the address it listens to (port 4000 by default). You can also try to restart your reverse proxy.
- `400 Bad Request` could mean that the format of your public endpoint is invalid. It needs to be prefixed with `https://` and not `http://`. Follow the same format as in the [Configuration](configuration.md) section. Note that if you use the Docker Compose setup, `VIRTUAL_HOST` and `LETSENCRYPT_HOST` should be without the `https://` prefix and `HAGALL_PUBLIC_ENDPOINT` should have the `https://` prefix.
