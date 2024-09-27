# Configuration

Configuration parameters can be passed to the Relay server either as environment variables or flags.

`./hagall --public-endpoint https://hagall.example.com` (where `https://hagall.example.com` is the public, external address where this Relay server is reachable) will launch the Relay server with sane defaults, but if you for some reason want to modify the default configuration of the Relay server you can run `./hagall -h` for a full list of parameters.

Here are some example parameters:

| Flag               | Environment variable    | Default | Example                    | Description                                                                                                                |
| ------------------ | ----------------------- | ------- | -------------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| --addr             | HAGALL_ADDR             | :4000   | :4000                      | Listening address for client connections. This is the port you want your reverse proxy to forward traffic to.              |
| --log-indent       | HAGALL_LOG_INDENT       | false   | true                       | Indent logs                                                                                                                |
| --public-endpoint  | HAGALL_PUBLIC_ENDPOINT  | _N/A_   | https://hagall.example.com | The public endpoint where this Relay server is reachable. This endpoint will be registered with Hagall Discovery Service. |
| --log-level        | HAGALL_LOG_LEVEL        | info    | debug                      | The log level (debug, info, warning or error)                                                                              |
| --private-key-file | HAGALL_PRIVATE_KEY_FILE | _N/A_   | hagall-private.key         | The file that contains the private key of a Relay server-unique Ethereum-compatible wallet                              |
| --private-key      | HAGALL_PRIVATE_KEY      | _N/A_   | 0x0                        | The private key of a Relay server-unique Ethereum-compatible wallet                              |

Every Relay server needs a unique wallet. You can generate it in a wallet app of your choice (such as MetaMask) and copy its private key to a file called `hagall-private.key`.
If you wish to generate a wallet on the command-line, make sure that you back up your private key (for example by adding it to your wallet app) to not lose your stake or rewards. Here is an example command to generate a wallet and save its private key to a file so the Relay server can use it:

```shell
pip3 install web3
python3 -c "from web3 import Web3; w3 = Web3(); acc = w3.eth.account.create(); print(f'{w3.to_hex(acc.key)}')" > hagall-private.key
chmod 400 hagall-private.key
```

We recommend that the private key is supplied as a file (`HAGALL_PRIVATE_KEY_FILE`) rather than directly on the command line or through environment variables (`HAGALL_PRIVATE_KEY`) as files are more secure.

**DO NOT CONFIGURE A WALLET WITH EXISTING ASSETS**, instead generate a new wallet for every Relay server you operate.
The private key of your wallet is only used by the Relay server for authentication and verification of your reputation deposit and will stay on your machine. But if someone gains access to the private key file on your server, they will get access to your wallet, so please take appropriate precautions.
