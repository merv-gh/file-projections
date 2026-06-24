package sample;

public class Tier {
    int bonus(int points, boolean member) {
        if (member) {
            points = points + 5;
        }
        return points;
    }
}
