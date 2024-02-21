# Troubleshooting

After launching Hagall, it's a good idea to take a look at the logs to make sure your server is registered, working and accessible. There are a few things you can look for:

- "hagall is successfully registered to hds" should show up in the log
- `"message":"new client is connected","tags":{"app-key":"0xSMOKE"}}` should show up in the log. These are health checks (also known as smoke tests) running from the central Hagall Discovery Service (HDS) to test that the server is working
- Check metrics like `ws_connected_clients` (see the [Metrics](metrics.md) document for details)
- Check that the server appears on the posemesh [dashboard](https://dashboard.posemesh.org/servers)

If registration to HDS fails, check the status code in the log message. The status code is the response from HDS when it tries to call your Hagall server.

- `"status":"504 Gateway Timeout"` or `403 Forbidden` could mean that HDS couldn't reach your Hagall instance because the connection to your public endpoint (URL) timed out. It could happen because you didn't do port forwarding in your router or didn't allow your web server / reverse proxy (such as nginx) or port 443 in your firewall. We have also seen cases where Internet Service Providers blocked common service ports like 80 and 443. Here's an example for [Orcon](https://help.orcon.net.nz/hc/en-us/articles/360005168154-Port-filtering-in-My-Orcon).
- You can test that your Hagall is reachable from the public Internet using [reqbin.com](https://reqbin.com/). Write your Hagall URL (the "public endpoint" address you configured) and press the Send button. If you get a status 400 (Bad Request) back, the text is green and the content says "not websocket protocol", everything is working as it should.
- `403 Forbidden` could also happen if your clock is out of sync. Sync it and try again. It could also happen if you run two Hagall servers at the same time with the same public endpoint (URL). Make sure you only run one Hagall with one unique public endpoint.
- `503 Service Unavailable` means that HDS reaches a reverse proxy (such as nginx), but the backend server (Hagall) is unavailable. Check the reverse proxy logs to verify that HDS actually reaches the reverse proxy you think it reaches. And make sure Hagall is running and that the reverse proxy points to Hagall using the address it listens to (port 4000 by default). You can also try to restart your reverse proxy.
- `400 Bad Request` could mean that the format of your public endpoint is invalid. It needs to be prefixed with `https://` and not `http://`. Follow the same format as in the [Configuration](configuration.md) section. Note that if you use the Docker Compose setup, `VIRTUAL_HOST` and `LETSENCRYPT_HOST` should be without the `https://` prefix and `HAGALL_PUBLIC_ENDPOINT` should have the `https://` prefix.
