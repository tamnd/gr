Feature: Match

  Scenario: Return a single node
    Given an empty graph
    And having executed:
      """
      CREATE (:Person {name: 'Alice'})
      """
    When executing query:
      """
      MATCH (n:Person) RETURN n.name AS name
      """
    Then the result should be:
      | name    |
      | 'Alice' |

  Scenario: Return multiple nodes
    Given an empty graph
    And having executed:
      """
      CREATE (:Person {name: 'Alice'})
      CREATE (:Person {name: 'Bob'})
      """
    When executing query:
      """
      MATCH (n:Person) RETURN n.name AS name ORDER BY name
      """
    Then the result should be, in order:
      | name    |
      | 'Alice' |
      | 'Bob'   |

  Scenario: Match returns empty result on empty graph
    Given an empty graph
    When executing query:
      """
      MATCH (n) RETURN n
      """
    Then the result should be empty

  Scenario: Match node by property
    Given an empty graph
    And having executed:
      """
      CREATE (:N {v: 42})
      CREATE (:N {v: 7})
      """
    When executing query:
      """
      MATCH (n:N) WHERE n.v = 42 RETURN n.v AS v
      """
    Then the result should be:
      | v  |
      | 42 |

  Scenario: Match with relationship
    Given an empty graph
    And having executed:
      """
      CREATE (:A {n: 1})-[:T]->(:B {n: 2})
      """
    When executing query:
      """
      MATCH (a:A)-[:T]->(b:B) RETURN a.n AS an, b.n AS bn
      """
    Then the result should be:
      | an | bn |
      | 1  | 2  |
