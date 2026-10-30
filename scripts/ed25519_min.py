import hashlib

p = 2 ** 255 - 19
q = 2 ** 252 + 27742317777372353535851937790883648493
d = -121665 * pow(121666, p - 2, p) % p
I = pow(2, (p - 1) // 4, p)


def _xrecover(y):
    xx = (y * y - 1) * pow(d * y * y + 1, p - 2, p) % p
    x = pow(xx, (p + 3) // 8, p)
    if (x * x - xx) % p != 0:
        x = x * I % p
    if x % 2 != 0:
        x = p - x
    return x


def _isedn(P):
    x, y, z, t = P
    return (-x * x + y * y - z * z - d * t * t) % p == 0


_Bx = _xrecover(4 * pow(5, p - 2, p) % p)
_By = 4 * pow(5, p - 2, p) % p
B = (_Bx, _By, 1, _Bx * _By % p)


def _add(P, Q):
    a = (P[1] - P[0]) * (Q[1] - Q[0]) % p
    b = (P[1] + P[0]) * (Q[1] + Q[0]) % p
    c = 2 * P[3] * Q[3] * d % p
    dd = 2 * P[2] * Q[2] % p
    e, f, g, h = b - a, dd - c, dd + c, b + a
    return (e * f % p, g * h % p, f * g % p, e * h % p)


def _mul(s, P):
    Q = (0, 1, 1, 0)
    while s > 0:
        if s & 1:
            Q = _add(Q, P)
        P = _add(P, P)
        s >>= 1
    return Q


def _enc(P):
    zinv = pow(P[2], p - 2, p)
    x = P[0] * zinv % p
    y = P[1] * zinv % p
    return int.to_bytes(y | ((x & 1) << 255), 32, "little")


def _dec(s):
    y = int.from_bytes(s, "little") & ((1 << 255) - 1)
    x = _xrecover(y)
    if (x & 1) != ((s[31] >> 7) & 1):
        x = p - x
    P = (x, y, 1, x * y % p)
    if not _isedn(P):
        raise ValueError("point not on curve")
    return P


def h512(data):
    return hashlib.sha512(data).digest()


def public_key(seed):
    if len(seed) != 32:
        raise ValueError("seed must be 32 bytes")
    a = int.from_bytes(h512(seed)[:32], "little")
    a &= (1 << 254) - 8
    a |= 1 << 254
    return _enc(_mul(a, B))


def sign(seed, msg):
    if len(seed) != 32:
        raise ValueError("seed must be 32 bytes")
    h = h512(seed)
    a = int.from_bytes(h[:32], "little")
    a &= (1 << 254) - 8
    a |= 1 << 254
    A = _enc(_mul(a, B))
    r = int.from_bytes(h512(h[32:] + msg), "little") % q
    R = _enc(_mul(r, B))
    k = int.from_bytes(h512(R + A + msg), "little") % q
    S = (r + k * a) % q
    return R + int.to_bytes(S, 32, "little")


def verify(pub, msg, sig):
    if len(pub) != 32 or len(sig) != 64:
        return False
    try:
        R = _dec(sig[:32])
        A = _dec(pub)
    except ValueError:
        return False
    S = int.from_bytes(sig[32:], "little")
    if S >= q:
        return False
    k = int.from_bytes(h512(sig[:32] + pub + msg), "little") % q
    return _enc(_mul(S, B)) == _enc(_add(R, _mul(k, A)))


if __name__ == "__main__":
    seed = bytes.fromhex("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
    exp_pub = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"
    exp_sig = (
        "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e06522490155"
        "5fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b"
    )
    pub = public_key(seed)
    sig = sign(seed, b"")
    assert pub.hex() == exp_pub, pub.hex()
    assert sig.hex() == exp_sig, sig.hex()
    assert verify(bytes.fromhex(exp_pub), b"", bytes.fromhex(exp_sig))
    assert not verify(bytes.fromhex(exp_pub), b"x", bytes.fromhex(exp_sig))
    print("RFC8032 TEST1 OK: pub/sig 与官方测试向量逐字节一致")
