# Security Troubleshooting Guide

This guide helps diagnose and resolve security-related issues in Beskar7.

## Common Security Issues

### TLS Certificate Problems

#### Issue: Certificate Validation Failures

**Symptoms:**
- PhysicalHost webhook validation errors
- Connection failures to BMC endpoints
- TLS certificate validation errors in logs

**Diagnosis:**
```bash
# Check PhysicalHost webhook events
kubectl get events -n beskar7-system --field-selector reason=TLSSecurityIssue

# Check certificate manually
openssl s_client -connect bmc.example.com:443 -verify_return_error

# Test TLS connection
curl -v https://bmc.example.com
```

**Common Causes & Solutions:**

1. **Self-signed certificates**
   ```yaml
   # Temporary fix for development
   spec:
     redfishConnection:
       insecureSkipVerify: true  # Only for development!
   
   # Proper fix: Add custom CA
   spec:
     redfishConnection:
       caCertificateSecretRef:
         name: custom-ca-cert
   ```

2. **Expired certificates**
   ```bash
   # Check certificate expiry
   echo | openssl s_client -connect bmc.example.com:443 2>/dev/null | openssl x509 -noout -dates
   
   # Solution: Renew BMC certificate
   ```

3. **Hostname mismatch**
   ```bash
   # Check certificate SANs
   echo | openssl s_client -connect bmc.example.com:443 2>/dev/null | openssl x509 -noout -text | grep -A1 "Subject Alternative Name"
   
   # Solution: Use correct hostname or update certificate
   ```

4. **CA certificate not trusted**
   ```yaml
   # Add custom CA certificate
   apiVersion: v1
   kind: Secret
   metadata:
     name: custom-ca-cert
   data:
     ca.crt: <base64-encoded-ca-cert>
   ```

#### Issue: Certificate Expiry Warnings

**Symptoms:**
- Warning events about certificate expiry
- Security monitor alerts

**Diagnosis:**
```bash
# Check certificate expiry events
kubectl get events -n beskar7-system --field-selector reason=TLSSecurityIssue,type=Warning

# View security report for certificate issues
kubectl logs deployment/controller-manager -n beskar7-system | grep "certificate.*expir"
```

**Solutions:**
```bash
# Plan certificate renewal
# 1. Generate new certificate
# 2. Update BMC with new certificate
# 3. Update custom CA if needed
```

### RBAC Permission Issues

#### Issue: Permission Denied Errors

**Symptoms:**
- Controller unable to create/update resources
- "forbidden" errors in logs
- RBAC security violations

**Diagnosis:**
```bash
# Check current permissions
kubectl auth can-i --list --as=system:serviceaccount:beskar7-system:controller-manager

# Check RBAC events
kubectl get events -n beskar7-system --field-selector reason=RBACSecurityIssue

# Verify ClusterRole
kubectl get clusterrole manager-role -o yaml

# Check ClusterRoleBinding
kubectl get clusterrolebinding manager-rolebinding -o yaml
```

**Common Causes & Solutions:**

1. **Missing permissions**
   ```yaml
   # Add missing permission to ClusterRole
   rules:
   - apiGroups: ["infrastructure.cluster.x-k8s.io"]
     resources: ["physicalhosts"]
     verbs: ["create"]  # Add missing verb
   ```

2. **Overly restrictive security policy**
   ```yaml
   # Update security policy to allow required permissions
   security:
     rbac:
       max_allowed_permissions:
         beskar7-controller:
           - apiGroups: ["new-api-group"]
             resources: ["new-resource"]
             verbs: ["get", "list"]
   ```

3. **Service account not bound to role**
   ```bash
   # Check binding
   kubectl get clusterrolebinding manager-rolebinding -o yaml
   
   # Fix: Ensure correct service account
   subjects:
   - kind: ServiceAccount
     name: controller-manager
     namespace: beskar7-system
   ```

#### Issue: Overly Broad Permissions Detected

**Symptoms:**
- Security monitor alerts about broad permissions
- RBAC security warnings

**Diagnosis:**
```bash
# Check RBAC security events
kubectl get events -n beskar7-system --field-selector reason=RBACSecurityIssue

# Review current permissions
kubectl get clusterrole manager-role -o yaml
```

**Solutions:**
```yaml
# Replace broad permissions with specific ones
# Bad:
- apiGroups: ["*"]
  resources: ["*"]
  verbs: ["*"]

# Good:
- apiGroups: ["infrastructure.cluster.x-k8s.io"]
  resources: ["physicalhosts", "beskar7machines"]
  verbs: ["get", "list", "watch", "create", "update", "patch"]
```

### Credential Security Issues

#### Issue: Credential Validation Failures

**Symptoms:**
- PhysicalHost webhook validation errors
- Secret validation failures
- Password policy violations

**Diagnosis:**
```bash
# Check credential events
kubectl get events -n beskar7-system --field-selector reason=SecretSecurityIssue

# Validate secret format
kubectl get secret bmc-credentials -o yaml

# Check password policy
kubectl get configmap beskar7-security-policy -n beskar7-system -o yaml
```

**Common Causes & Solutions:**

1. **Missing required fields**
   ```yaml
   # Ensure secret has required fields
   apiVersion: v1
   kind: Secret
   metadata:
     name: bmc-credentials
   data:
     username: <base64-encoded>  # Required
     password: <base64-encoded>  # Required
   ```

2. **Weak password**
   ```bash
   # Generate strong password
   PASSWORD=$(openssl rand -base64 32)
   kubectl patch secret bmc-credentials --patch='{"data":{"password":"'$(echo -n "$PASSWORD" | base64)'"}}'
   ```

3. **Old credentials**
   ```bash
   # Check credential age
   kubectl get secret bmc-credentials -o jsonpath='{.metadata.creationTimestamp}'
   
   # Rotate credentials if needed
   ```

#### Issue: Authentication Failures to BMC

**Symptoms:**
- Redfish authentication errors
- BMC connection failures
- Invalid credentials errors

**Diagnosis:**
```bash
# Test credentials manually
USERNAME=$(kubectl get secret bmc-credentials -o jsonpath='{.data.username}' | base64 -d)
PASSWORD=$(kubectl get secret bmc-credentials -o jsonpath='{.data.password}' | base64 -d)
curl -k -u "$USERNAME:$PASSWORD" https://bmc.example.com/redfish/v1/Systems

# Check PhysicalHost status
kubectl get physicalhost server-01 -o yaml
```

**Solutions:**
1. Verify credentials are correct
2. Check BMC user account status
3. Ensure account has required permissions
4. Rotate credentials if compromised

### Network Security Issues

#### Issue: Network Policy Blocking Traffic

**Symptoms:**
- Connection timeouts
- Network connectivity failures
- Webhook failures

**Diagnosis:**
```bash
# Check network policies
kubectl get networkpolicy -n beskar7-system

# Check pod connectivity
kubectl exec -it deployment/controller-manager -n beskar7-system -- curl -v https://bmc.example.com

# Check network policy logs (if supported)
```

**Solutions:**
```yaml
# Add missing egress rule for BMC communication
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-bmc-egress
spec:
  podSelector:
    matchLabels:
      control-plane: controller-manager
  policyTypes:
  - Egress
  egress:
  - to: []
    ports:
    - protocol: TCP
      port: 443
```

#### Issue: Webhook Certificate Problems

**Symptoms:**
- Webhook validation failures
- TLS handshake errors
- Certificate verification errors

**Diagnosis:**
```bash
# Check webhook certificate
kubectl get secret beskar7-webhook-server-cert -n beskar7-system -o yaml

# Check webhook configuration
kubectl get validatingwebhookconfigurations.admissionregistration.k8s.io

# Test webhook manually
kubectl create -f test-physicalhost.yaml --dry-run=server
```

**Solutions:**
```bash
# Recreate webhook certificates
kubectl delete secret beskar7-webhook-server-cert -n beskar7-system
# Restart manager to regenerate certs
kubectl rollout restart deployment/beskar7-controller-manager -n beskar7-system
```

### Container Security Issues

#### Issue: Security Context Violations

**Symptoms:**
- Pod creation failures
- Security policy violations
- Container startup errors

**Diagnosis:**
```bash
# Check pod security violations
kubectl get events -n beskar7-system --field-selector type=Warning

# Check pod security context
kubectl get pod -l control-plane=controller-manager -n beskar7-system -o yaml
```

**Solutions:**
```yaml
# Fix security context
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: ["ALL"]
```

#### Issue: Resource Limit Violations

**Symptoms:**
- OOMKilled pods
- CPU throttling
- Performance issues

**Diagnosis:**
```bash
# Check resource usage
kubectl top pod -l control-plane=beskar7-controller-manager -n beskar7-system

# Check resource limits
kubectl get pod -l control-plane=controller-manager -n beskar7-system -o yaml | grep -A5 resources
```

**Solutions:**
```yaml
# Increase resource limits
resources:
  limits:
    cpu: 1000m      # Increase from 500m
    memory: 1Gi     # Increase from 512Mi
  requests:
    cpu: 200m       # Increase from 100m
    memory: 256Mi   # Increase from 128Mi
```

## Security Monitoring Issues

### Issue: Security Monitor Not Running

**Symptoms:**
- No security events generated
- Missing security metrics
- No security reports

**Diagnosis:**
```bash
# Check if security monitoring is enabled
kubectl get deployment controller-manager -n beskar7-system -o yaml | grep enable-security-monitoring

# Check manager logs
kubectl logs deployment/controller-manager -n beskar7-system | grep security

# Check security monitor status
kubectl get events -n beskar7-system | grep security
```

**Solutions:**
```bash
# Enable security monitoring
kubectl patch deployment controller-manager -n beskar7-system \
  --patch '{"spec":{"template":{"spec":{"containers":[{"name":"manager","args":["--leader-elect","--enable-security-monitoring=true"]}]}}}}'

# Restart manager
kubectl rollout restart deployment/controller-manager -n beskar7-system
```

### Issue: False Positive Security Alerts

**Symptoms:**
- Excessive security warnings
- Valid configurations flagged as violations
- Alert fatigue

**Diagnosis:**
```bash
# Review security events
kubectl get events -n beskar7-system --field-selector type=Warning

# Check security policy configuration
kubectl get configmap beskar7-security-policy -n beskar7-system -o yaml
```

**Solutions:**
```yaml
# Adjust security policy thresholds
security:
  tls:
    expiry_warning_days: 15  # Reduce from 30 days
  
  credentials:
    rotation_policy:
      max_age_days: 180      # Increase from 90 days
  
  monitoring:
    security_monitoring:
      alert_on_medium: false # Disable medium alerts
```

## Debugging Tools and Commands

### Security Status Commands

```bash
# Overall security status
kubectl get events -n beskar7-system --field-selector type=Warning

# TLS security status
kubectl get events -n beskar7-system --field-selector reason=TLSSecurityIssue

# RBAC security status
kubectl get events -n beskar7-system --field-selector reason=RBACSecurityIssue

# Credential security status
kubectl get events -n beskar7-system --field-selector reason=SecretSecurityIssue

# Network policy status
kubectl get networkpolicy -n beskar7-system

# Security policy
kubectl get configmap beskar7-security-policy -n beskar7-system -o yaml
```

### Certificate Debugging

```bash
# Check certificate details
openssl s_client -connect bmc.example.com:443 -showcerts

# Check certificate chain
openssl s_client -connect bmc.example.com:443 -verify_return_error

# Check certificate expiry
echo | openssl s_client -connect bmc.example.com:443 2>/dev/null | openssl x509 -noout -dates

# Check certificate SANs
echo | openssl s_client -connect bmc.example.com:443 2>/dev/null | openssl x509 -noout -text | grep -A1 "Subject Alternative Name"
```

### RBAC Debugging

```bash
# Check current permissions
kubectl auth can-i --list --as=system:serviceaccount:beskar7-system:controller-manager

# Check specific permission
kubectl auth can-i create physicalhosts --as=system:serviceaccount:beskar7-system:controller-manager

# List all ClusterRoles
kubectl get clusterrole | grep beskar7

# Check ClusterRoleBinding
kubectl get clusterrolebinding | grep beskar7
```

### Network Debugging

```bash
# Test network connectivity
kubectl exec -it deployment/controller-manager -n beskar7-system -- curl -v https://bmc.example.com

# Check DNS resolution
kubectl exec -it deployment/controller-manager -n beskar7-system -- nslookup bmc.example.com

# Check network policies
kubectl get networkpolicy -n beskar7-system -o yaml

# Test webhook connectivity
kubectl exec -it deployment/controller-manager -n beskar7-system -- curl -v https://kubernetes.default.svc/api/v1
```

## Log Analysis

### Enable Debug Logging

```bash
# Enable verbose logging
kubectl patch deployment controller-manager -n beskar7-system \
  --patch '{"spec":{"template":{"spec":{"containers":[{"name":"manager","args":["--leader-elect","--v=2"]}]}}}}'

# Enable security debug logging
kubectl patch deployment controller-manager -n beskar7-system \
  --patch '{"spec":{"template":{"spec":{"containers":[{"name":"manager","env":[{"name":"BESKAR7_SECURITY_DEBUG","value":"true"}]}]}}}}'
```

### Log Patterns to Look For

```bash
# TLS errors
kubectl logs deployment/controller-manager -n beskar7-system | grep -i "tls\|certificate\|x509"

# RBAC errors
kubectl logs deployment/controller-manager -n beskar7-system | grep -i "forbidden\|rbac\|permission"

# Credential errors
kubectl logs deployment/controller-manager -n beskar7-system | grep -i "auth\|credential\|password"

# Security violations
kubectl logs deployment/controller-manager -n beskar7-system | grep -i "security\|violation\|policy"
```

## Emergency Procedures

### Disable Security Features

If security features are blocking operation:

```bash
# Disable security monitoring temporarily
kubectl patch deployment controller-manager -n beskar7-system \
  --patch '{"spec":{"template":{"spec":{"containers":[{"name":"manager","args":["--leader-elect","--enable-security-monitoring=false"]}]}}}}'

# Allow insecure TLS temporarily (development only)
kubectl patch physicalhost server-01 \
  --patch '{"spec":{"redfishConnection":{"insecureSkipVerify":true}}}'

# Disable network policies temporarily
kubectl delete networkpolicy --all -n beskar7-system
```

### Recovery from Security Lockout

If RBAC changes lock out the controller:

```bash
# Reset to minimal required permissions
kubectl apply -f config/rbac/role.yaml

# Restart controller
kubectl rollout restart deployment/controller-manager -n beskar7-system

# Verify permissions
kubectl auth can-i --list --as=system:serviceaccount:beskar7-system:controller-manager
```

## Performance Impact

Security features may impact performance:

### Monitoring Performance

```bash
# Check resource usage
kubectl top pod -l control-plane=controller-manager -n beskar7-system

# Monitor security scan duration
kubectl logs deployment/beskar7-controller-manager -n beskar7-system | grep "security scan"

# Check metrics
curl http://localhost:8080/metrics | grep beskar7_security
```

### Optimization Recommendations

1. **Reduce scan frequency** for non-critical environments
2. **Disable unnecessary security checks** in development
3. **Increase resource limits** if security monitoring causes resource pressure
4. **Use caching** for TLS certificate validation

## Getting Help

### Collect Diagnostic Information

```bash
#!/bin/bash
# Security diagnostic script

echo "=== Beskar7 Security Diagnostics ==="
echo "Date: $(date)"
echo

echo "=== Security Events ==="
kubectl get events -n beskar7-system --field-selector type=Warning

echo "=== Security Policy ==="
kubectl get configmap beskar7-security-policy -n beskar7-system -o yaml

echo "=== RBAC Configuration ==="
kubectl get clusterrole manager-role -o yaml
kubectl get clusterrolebinding manager-rolebinding -o yaml

echo "=== Network Policies ==="
kubectl get networkpolicy -n beskar7-system -o yaml

echo "=== Controller Logs (last 100 lines) ==="
kubectl logs deployment/beskar7-controller-manager -n beskar7-system --tail=100

echo "=== Controller Status ==="
kubectl get deployment beskar7-controller-manager -n beskar7-system -o yaml
```

### Contact Support

When reporting security issues, include:

1. Diagnostic information from the script above
2. Description of the issue and steps to reproduce
3. Environment details (Kubernetes version, deployment method)
4. Any custom security configurations
5. Timeline of when the issue started

### Community Resources

- **GitHub Issues**: https://github.com/projectbeskar/beskar7/issues
- **Security Advisory**: security@beskar7.io
- **Community Slack**: #beskar7-security
- **Documentation**: https://docs.beskar7.io/security/ 