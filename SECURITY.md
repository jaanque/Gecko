# Política de seguretat

La seguretat és el nucli de **Gecko**. Agraïm enormement qualsevol revisió responsable del codi o del protocol.

## Versions amb suport

| Versió | Suport de seguretat |
| :--- | :--- |
| `main` / `1.x` | Actiu |

## Com informar d'una vulnerabilitat

**No obris una Issue pública** per informar de vulnerabilitats de seguretat (p. ex. omissió d'autenticació, injecció sense consentiment, *path traversal*, filtració de claus o DoS).

1. Utilitza **GitHub Security Advisories** (*Security → Report a vulnerability*) en aquest repositori per comunicar l'error de manera privada.
2. Inclou, si és possible:
   - Component afectat (`RelayServidor`, `AdminRemoto`, `ClientRemot`, protocol).
   - Passos per reproduir-lo o prova de concepte.
   - Impacte estimat i proposta de mitigació (opcional).

## Model de seguretat de Gecko

- Totes les connexions utilitzen **TLS 1.3 mutu (mTLS)** amb *pinning* SHA-256 en temps constant i tokens de 256 bits separats per rol (`admin` i `client`).
- Cada client generat connectat al Relay obté una credencial única revocable individualment.
- La injecció de ratolí, teclat i accions de sistema al client resta bloquejada per defecte fins que l'usuari final accepta explícitament la sessió.
- Les transferències de fitxers sanejament noms (*anti-traversal*), limiten la mida a 50 MB i verifiquen la suma SHA-256 abans de desar el fitxer final.
