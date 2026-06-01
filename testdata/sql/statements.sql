SELECT user_id, email FROM users WHERE user_id = 42;
DROP TABLE IF EXISTS old_users;
CREATE INDEX idx_users_email ON users(email);
INSERT INTO orders(customer_id, user_id) VALUES (7, 42);
