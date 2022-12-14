```
                                    ____   _                __        __ 
                                   / __ \ (_)____   ____   / /____ _ / /_
                                  / /_/ // // __ \ / __ \ / // __ `// __/
                                 / ____// // /_/ // /_/ // // /_/ // /_  
                                /_/    /_/ \____// .___//_/ \__,_/ \__/  
                                                /_/                      
```

## Get Started

Pioplat is a scalable and customizable framework for reducing blockchain latency that excels with light weight, 
efficiency, and low cost. It is an open-sourced, distributed, elastic framework for winning the blockchain latency war. 
Currently, it only supports Binance Smart Chain (`bsc`) is  supported, Ethereum (`eth`) and Polygon(`matic`) will be 
supported in the future. 

### BSC

The Pioplat system consists of two classes of nodes, first a Pioplat full node, which is a modified `bsc` used to 
support relay nodes, which can be any number, and the more deployed, the lower the latency.

### Build 

**build modified full node**
```bash
git clone https://github.com/wangtsiao/bsc-pioplat.git
cd bsc-pioplat
make geth
```
After that you should refer to the BSC [documentation](https://docs.binance.org/smart-chain/developer/fullnode.html) to 
get this full node up and running. It is recommended to use [snapshot](https://github.com/bnb-chain/bsc-snapshots), 
which greatly reduces the time to synchronize to the latest state.


**build relay node**
```bash
git clone https://github.com/wangtsiao/pioplat.git
cd pioplat-core/cmd/piobsc
go build
```
You should deploy these relay nodes on different continents so that you can minimize latency as low as possible.

### TLS certificate

Generate a self-signed certificate for `TLS` secure communication between the relay node and the full node. 
Here is an example with my information.
```bash
# 1. Generate CA's private key and self-signed certificate
# password: set anything you want
openssl req -x509 -newkey rsa:4096 -days 365 -keyout ca-key.pem -out ca-cert.pem -subj "/C=CN/ST=Beijing/L=Beijing/O=Peking University/OU=Education/CN=*.pku.edu.cn/emailAddress=wangtsiao@stu.pku.edu.cn"
openssl x509 -in ca-cert.pem -noout -text


# 2. Generate web server's private key and CSR
# password: set anything you want
openssl req -newkey rsa:4096 -keyout server-key.pem -out server-req.pem -subj "/C=CN/ST=Beijing/L=Beijing/O=Pioplat/OU=Computer/CN=*.pioplat.cloud/emailAddress=office@pioplat.cloud"


# 3. Sign the web server's certificate request
# write below info to server-ext.cnf file
# subjectAltName=DNS:*.pioplat.cloud,IP:0.0.0.0
openssl x509 -req -in server-req.pem -days 60 -CA ca-cert.pem -CAkey ca-key.pem -CAcreateserial -out server-cert.pem -extfile server-ext.cnf

# remove key password
openssl rsa -in [original.key] -out [new.key]
```

### Set up
The necessary parameters are located in the `config.toml` file. You can customize these parameters.
```bash
DisguiseServerUrl = "<full node ip and port>" # Network address of the full node
PioplatServerListenAddr = "0.0.0.0:port" # Port for communication between relay nodes
PioplatAesKeyIvHex = "0xe7fb42c1326792d0647d108f4fbbafcfd84ac07fdbbb770a760b15892574222e" # AES-CTR Key and IV for secure communication between relay nodes
PioplatRpcAdminSecret = "dvXHCxTswxPsRjvtCfRdEUMbSll4tSm7" # the http token parameter of the relay node
```
### Cite us


