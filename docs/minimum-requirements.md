# Minimum Requirements

Most modern computers will be able to run Hagall. We have tested on desktops, laptops, servers and Raspberry Pis.

- An x86, x86_64 or ARMv6+
- At least 64 MiB of RAM
- At least 20 MiB of disk space
- A supported operating system, we currently provide pre-compiled binaries for Windows, macOS, Linux, FreeBSD, Solaris as well as Docker images

Additionally, you need this in order to expose Hagall to the Internet:

- A web server or reverse proxy which
  - is compatible with HTTPS and WebSockets (HTTP/1.1 or later)
  - has an SSL certificate installed
- A stable Internet connection with
  - an externally accessible, public IP address for your reverse proxy to listen to
  - at least 10 Mbps downstream and upstream
- A domain name configured to point to your IP address

You can use our Docker Compose or Kubernetes setup that includes a basic nginx reverse proxy with a Let's Encrypt issued SSL certificate.

Auki's Hagall Discovery Service will perform regular checks on the health of your server to determine if it's fit to serve traffic. Make sure that you have enough spare compute capacity and bandwidth for hosting sessions, or your server might be delisted from the network.
