# Read Bitcoin from your own node

By default Fundarr reads Bitcoin and Litecoin wallets from public servers (mempool.space, then blockstream.info). They answer every address of your accounts, so they can link your whole holdings to your IP, and they limit how fast they answer. With your own node, nothing leaves your network and there is no rate limit.

Fundarr talks to an **Esplora API**: the REST API of mempool and of electrs. It does not speak to Bitcoin Core directly, nor the Electrum protocol.

## Where to paste it

On a Wallet source, open **Advanced** and paste the address in **Bitcoin server** (or **Litecoin server** for a Litecoin node). Saving checks that the server answers, that it is an Esplora API, and that it serves the main network, by asking it for block 0 and comparing it with Bitcoin's genesis block. Once a server is set, Fundarr only uses it: if it stops answering, the sync fails instead of falling back to a public server.

The address is the API, not the web page: for mempool it is the site's address followed by `/api`.

## Umbrel

1. Install **mempool** from the App Store. It needs Bitcoin Node and Electrs, which Umbrel installs with it; wait until both are fully synced (Electrs can take a day or more after Bitcoin Node).
2. Open mempool once to check that it shows your node's blocks.
3. In Fundarr, paste:

   ```
   http://<umbrel-ip>:3006/api
   ```

   `3006` is the port Umbrel gives the mempool app. Use the device's IP address (`192.168.1.20`, say) rather than `umbrel.local`: inside a Docker container, `.local` names often do not resolve. Write `http://` explicitly: without a scheme, Fundarr assumes `https://`, which Umbrel's apps do not serve on the local network.

## Start9

1. Install **Bitcoin Core**, **electrs** and **mempool** from the Marketplace, and wait until they are synced. mempool must use electrs (or Fulcrum) as its address backend: without one, it cannot look up addresses.
2. On the mempool service page, open **Interfaces** and copy its **LAN** address (`https://<something>.local`).
3. In Fundarr, paste that address followed by `/api`.

StartOS serves its services over HTTPS with its own certificate authority, which Fundarr does not trust by default. Download the StartOS root CA (Start9's guide *Trusting your Root CA* shows where), put it in a folder next to Fundarr, mount it and point `SSL_CERT_DIR` to it; Fundarr then trusts it on top of the usual authorities:

```yaml
services:
  fundarr:
    environment:
      SSL_CERT_DIR: /certs
    volumes:
      - ./certs:/certs:ro   # holds the StartOS root CA (.crt)
```

`.local` names may not resolve inside a container either. If the check says the server cannot be reached, use the address StartOS shows for the LAN by IP, if your version offers one, or add the name to the container (`extra_hosts`).

## Another server

Any mempool or electrs instance works: mempool's address followed by `/api`, or electrs' HTTP address as it is (usually `http://<host>:3000`, with no suffix). For Litecoin, a mempool instance built for Litecoin (as litecoinspace.org runs) goes in **Litecoin server**.

## When the check fails

- **Cannot reach the server**: the address, the port, or a `.local` name the container cannot resolve (use the IP).
- **Not an Esplora API**: usually the web page's address without `/api`.
- **Wrong network**: the server serves testnet, signet or regtest. Fundarr only reads the main network.
- **Certificate error** (Start9): the root CA is not mounted, or `SSL_CERT_DIR` does not point to it.
