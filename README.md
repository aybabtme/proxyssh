# proxyssh
proxyssh: proxy connections over ssh to multiple hosts

# Usage:

Proxy as user `root` on remote port `3306`, for 3 MySQL hosts.
```
$ proxyssh -u root -rp 3306 mysql01.prod.com mysql02.prod.com mysql03.prod.com
proxyssh: connecting with user "root"
(50208) mysql01.prod.com: proxy started on "127.0.0.1:50208"
(50209) mysql02.prod.com: proxy started on "127.0.0.1:50209"
(50210) mysql03.prod.com: proxy started on "127.0.0.1:50210"
(50209) mysql02.prod.com: connection started...
(50209) mysql02.prod.com: connection finished...
```
