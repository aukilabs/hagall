# Testing

After launching Hagall, it's a good idea to take a look at the logs to make sure your server is registered, working and accessible. There are a few things you can look for:

* "hagall is successfully registered to hds" should show up in the log
* `"message":"new client is connected","tags":{"app-key":"0xSMOKE"}}` should show up in the log. These are health checks (also known as smoke tests) running from the central Hagall Discovery Service (HDS) to test that the server is working
* You can also check metrics like `ws_connected_clients`, see more [doc/metrics.md](Metrics).
