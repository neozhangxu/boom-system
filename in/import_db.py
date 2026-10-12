import socket

# 由于未安装 pymysql，我们可以通过标准库检查 3306 端口是否畅通
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
result = s.connect_ex(('127.0.0.1', 3306))
if result == 0:
    print("=== 3306 端口连通正常！ ===")
else:
    print("=== 无法连接到 3306 端口，请检查 1Panel 防火墙或端口映射 ===")
s.close()
